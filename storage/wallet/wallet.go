package wallet

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/filecoin-project/go-address"

	"github.com/asaskevich/EventBus"
	wallet_api "github.com/filecoin-project/venus/venus-shared/api/wallet"
	"github.com/filecoin-project/venus/venus-shared/types"
	logging "github.com/ipfs/go-log/v2"

	c "github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/venus-wallet/crypto"
	"github.com/filecoin-project/venus-wallet/crypto/aes"
	"github.com/filecoin-project/venus-wallet/storage"
)

var log = logging.Logger("wallet")

type GetPwdFunc func() string

var _ wallet_api.IWallet = &wallet{}

// wallet implementation
type wallet struct {
	keyCache map[string]crypto.PrivateKey // simple key cache
	ws       storage.KeyStore             // key storage
	mw       storage.KeyMiddleware        //
	bus      EventBus.Bus
	filter   ISignMsgFilter
	m        sync.RWMutex
}

func NewWallet(ks storage.KeyStore, mw storage.KeyMiddleware, filter ISignMsgFilter, bus EventBus.Bus, getPwd GetPwdFunc) wallet_api.ILocalWallet {
	w := &wallet{
		ws:       ks,
		mw:       mw,
		bus:      bus,
		filter:   filter,
		keyCache: make(map[string]crypto.PrivateKey),
	}
	if getPwd != nil {
		if pwd := getPwd(); len(pwd) != 0 {
			if err := w.SetPassword(context.Background(), pwd); err != nil {
				log.Fatalf("set password(%s) failed %v", pwd, err)
			}
		}
	}

	return w
}

func (w *wallet) SetPassword(ctx context.Context, password string) error {
	if err := w.checkPassword(ctx, password); err != nil {
		return err
	}
	return w.mw.SetPassword(ctx, password)
}

func (w *wallet) checkPassword(ctx context.Context, password string) error {
	hashPasswd := aes.Keccak256([]byte(password))
	addrs, err := w.WalletList(ctx)
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		key, err := w.ws.Get(addr)
		if err != nil {
			return err
		}
		_, err = w.mw.Decrypt(hashPasswd, key)
		if err != nil {
			return err
		}
	}

	return nil
}

func (w *wallet) Unlock(ctx context.Context, password string) error {
	if err := w.checkPassword(ctx, password); err != nil {
		return err
	}
	return w.mw.Unlock(ctx, password)
}

func (w *wallet) Lock(ctx context.Context, password string) error {
	return w.mw.Lock(ctx, password)
}

func (w *wallet) LockState(ctx context.Context) bool {
	return w.mw.LockState(ctx)
}

func (w *wallet) WalletNew(ctx context.Context, kt types.KeyType) (address.Address, error) {
	if err := w.mw.Next(); err != nil {
		return address.Undef, err
	}
	err := w.mw.CheckToken(ctx)
	if err != nil {
		return address.Undef, err
	}
	prv, err := crypto.GeneratePrivateKey(types.KeyType2Sign(kt))
	if err != nil {
		return address.Undef, err
	}
	addr, err := prv.Address()
	if err != nil {
		return address.Undef, err
	}
	ckey, err := w.mw.Encrypt(storage.EmptyPassword, prv)
	if err != nil {
		return address.Undef, err
	}
	err = w.ws.Put(ckey)
	if err != nil {
		return address.Undef, err
	}
	// notify
	w.bus.Publish("wallet:add_address", addr)
	return addr, nil
}

func (w *wallet) WalletHas(ctx context.Context, address address.Address) (bool, error) {
	return w.ws.Has(address)
}

func (w *wallet) WalletList(ctx context.Context) ([]address.Address, error) {
	addrs, err := w.ws.List()
	if err != nil {
		return nil, err
	}
	return addrs, nil
}

func (w *wallet) WalletSign(ctx context.Context, signer address.Address, toSign []byte, meta types.MsgMeta) (*c.Signature, error) {
	if err := w.mw.Next(); err != nil {
		return nil, err
	}
	var (
		owner address.Address
		data  []byte
	)
	// Do not validate strategy
	if meta.Type == types.MTVerifyAddress {
		_, toSign, err := GetSignBytes(toSign, meta)
		if err != nil {
			return nil, fmt.Errorf("get sign bytes failed: %v", err)
		}
		owner = signer
		data = toSign
	} else if meta.Type == types.MTChainMsg {
		if len(meta.Extra) == 0 {
			return nil, errors.New("msg type must contain extra data")
		}
		msg, err := types.DecodeMessage(meta.Extra)
		if err != nil {
			return nil, err
		}

		//Check filter
		err = w.filter.CheckSignMsg(ctx, SignMsg{
			SignType: types.MTChainMsg,
			Data:     msg,
		})
		if err != nil {
			return nil, err
		}

		owner = msg.From
		if signer.String() != owner.String() {
			return nil, fmt.Errorf("singe %s does not match from in MSG %s", signer, owner)
		}
		data = toSign
	} else {
		signObj, toSign, err := GetSignBytes(toSign, meta)
		if err != nil {
			return nil, fmt.Errorf("get sign bytes failed: %w", err)
		}

		//Check filter
		err = w.filter.CheckSignMsg(ctx, SignMsg{
			SignType: meta.Type,
			Data:     signObj,
		})
		if err != nil {
			return nil, err
		}
		owner = signer
		data = toSign
	}
	prvKey := w.cacheKey(owner)
	if prvKey == nil {
		key, err := w.ws.Get(owner)
		if err != nil {
			return nil, err
		}
		prvKey, err = w.mw.Decrypt(storage.EmptyPassword, key)
		if err != nil {
			return nil, err
		}
		w.pushCache(owner, prvKey)
	}
	return prvKey.Sign(data)
}

func (w *wallet) WalletExport(ctx context.Context, addr address.Address) (*types.KeyInfo, error) {
	if err := w.mw.Next(); err != nil {
		return nil, err
	}
	key, err := w.ws.Get(addr)
	if err != nil {
		return nil, err
	}
	pkey, err := w.mw.Decrypt(storage.EmptyPassword, key)
	if err != nil {
		return nil, err
	}
	return pkey.ToKeyInfo(), nil
}

func (w *wallet) WalletImport(ctx context.Context, ki *types.KeyInfo) (address.Address, error) {
	if err := w.mw.Next(); err != nil {
		return address.Undef, err
	}
	err := w.mw.CheckToken(ctx)
	if err != nil {
		return address.Undef, err
	}
	pk, err := crypto.NewKeyFromKeyInfo(ki)
	if err != nil {
		return address.Undef, err
	}
	addr, err := pk.Address()
	if err != nil {
		return address.Undef, err
	}
	exist, err := w.ws.Has(addr)
	if err != nil {
		return address.Undef, err
	}
	if exist {
		return addr, nil
	}
	key, err := w.mw.Encrypt(storage.EmptyPassword, pk)
	if err != nil {
		return address.Undef, err
	}
	err = w.ws.Put(key)
	if err != nil {
		return address.Undef, err
	}
	// notify
	w.bus.Publish("wallet:add_address", addr)
	return addr, nil
}

func (w *wallet) WalletDelete(ctx context.Context, addr address.Address) error {
	if err := w.mw.Next(); err != nil {
		return err
	}
	err := w.mw.CheckToken(ctx)
	if err != nil {
		return err
	}
	err = w.ws.Delete(addr)
	if err != nil {
		return err
	}
	w.pullCache(addr)
	w.bus.Publish("wallet:remove_address", addr)
	return nil
}

func (w *wallet) VerifyPassword(ctx context.Context, password string) error {
	if err := w.mw.Next(); err != nil {
		return err
	}
	return w.mw.VerifyPassword(ctx, password)
}

func (w *wallet) pushCache(address address.Address, prv crypto.PrivateKey) {
	w.m.Lock()
	defer w.m.Unlock()
	w.keyCache[address.String()] = prv
}

func (w *wallet) pullCache(address address.Address) {
	w.m.Lock()
	defer w.m.Unlock()
	delete(w.keyCache, address.String())
}

func (w *wallet) cacheKey(address address.Address) crypto.PrivateKey {
	w.m.RLock()
	defer w.m.RUnlock()
	return w.keyCache[address.String()]
}
