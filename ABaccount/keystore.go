// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package keystore implements encrypted storage of secp256k1 private keys.
//
// Keys are stored as encrypted JSON files according to the Web3 Secret Storage specification.
// See https://github.com/ethereum/wiki/wiki/Web3-Secret-Storage-Definition for more information.
package ABaccount

import (
	"crypto/ecdsa"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/usechain/go-usechain/accounts"
	"github.com/usechain/go-usechain/common"
	"github.com/usechain/go-usechain/common/hexutil"
	"github.com/usechain/go-usechain/common/math"

	"github.com/usechain/go-usechain/core/types"
	"github.com/usechain/go-usechain/crypto"
	"github.com/usechain/go-usechain/event"

	"github.com/usechain/go-usechain/core/state"

	"encoding/hex"
)

var (
	ErrLocked  = accounts.NewAuthNeededError("password or unlock")
	ErrNoMatch = errors.New("no key for given address or file")
	ErrDecrypt = errors.New("could not decrypt key with given passphrase")
)

// KeyStoreType is the reflect type of a keystore backend.
var KeyStoreType = reflect.TypeOf(&KeyStore{})

// KeyStoreScheme is the protocol scheme prefixing account and wallet URLs.
var KeyStoreScheme = "keystore"

// Maximum time between wallet refreshes (if filesystem notifications don't work).
const walletRefreshCycle = 3 * time.Second

// KeyStore manages a key storage directory on disk.
type KeyStore struct {
	storage  keyStore                     // Storage backend, might be cleartext or encrypted
	cache    *accountCache                // In-memory account cache over the filesystem storage
	changes  chan struct{}                // Channel receiving change notifications from the cache
	unlocked map[common.Address]*unlocked // Currently unlocked account (decrypted private keys)

	wallets     []accounts.Wallet       // Wallet wrappers around the individual key files
	updateFeed  event.Feed              // Event feed to notify wallet additions/removals
	updateScope event.SubscriptionScope // Subscription scope tracking current live listeners
	updating    bool                    // Whether the event notification loop is running

	mu sync.RWMutex
}

type unlocked struct {
	*Key
	abort chan struct{}
}

// NewKeyStore creates a keystore for the given directory.
func NewKeyStore(keydir string, scryptN, scryptP int) *KeyStore {
	keydir, _ = filepath.Abs(keydir)
	ks := &KeyStore{storage: &keyStorePassphrase{keydir, scryptN, scryptP}}
	ks.init(keydir)
	return ks
}

// NewPlaintextKeyStore creates a keystore for the given directory.
// Deprecated: Use NewKeyStore.
func NewPlaintextKeyStore(keydir string) *KeyStore {
	keydir, _ = filepath.Abs(keydir)
	ks := &KeyStore{storage: &keyStorePlain{keydir}}
	ks.init(keydir)
	return ks
}

func (ks *KeyStore) init(keydir string) {
	// Lock the mutex since the account cache might call back with events
	ks.mu.Lock()
	defer ks.mu.Unlock()

	// Initialize the set of unlocked keys and the account cache
	ks.unlocked = make(map[common.Address]*unlocked)
	ks.cache, ks.changes = newAccountCache(keydir)

	// TODO: In order for this finalizer to work, there must be no references
	// to ks. addressCache doesn't keep a reference but unlocked keys do,
	// so the finalizer will not trigger until all timed unlocks have expired.
	runtime.SetFinalizer(ks, func(m *KeyStore) {
		m.cache.close()
	})
	// Create the initial list of wallets from the cache
	accs := ks.cache.accounts()
	ks.wallets = make([]accounts.Wallet, len(accs))
	for i := 0; i < len(accs); i++ {
		ks.wallets[i] = &keystoreWallet{account: accs[i], keystore: ks}
	}
}

// Wallets implements accounts.Backend, returning all single-key wallets from the
// keystore directory.
func (ks *KeyStore) Wallets() []accounts.Wallet {
	// Make sure the list of wallets is in sync with the account cache
	ks.refreshWallets()

	ks.mu.RLock()
	defer ks.mu.RUnlock()

	cpy := make([]accounts.Wallet, len(ks.wallets))
	copy(cpy, ks.wallets)
	return cpy
}

// refreshWallets retrieves the current account list and based on that does any
// necessary wallet refreshes.
func (ks *KeyStore) refreshWallets() {
	// Retrieve the current list of accounts
	ks.mu.Lock()
	accs := ks.cache.accounts()

	// Transform the current list of wallets into the new one
	wallets := make([]accounts.Wallet, 0, len(accs))
	events := []accounts.WalletEvent{}

	for _, account := range accs {
		// Drop wallets while they were in front of the next account
		for len(ks.wallets) > 0 && ks.wallets[0].URL().Cmp(account.URL) < 0 {
			events = append(events, accounts.WalletEvent{Wallet: ks.wallets[0], Kind: accounts.WalletDropped})
			ks.wallets = ks.wallets[1:]
		}
		// If there are no more wallets or the account is before the next, wrap new wallet
		if len(ks.wallets) == 0 || ks.wallets[0].URL().Cmp(account.URL) > 0 {
			wallet := &keystoreWallet{account: account, keystore: ks}

			events = append(events, accounts.WalletEvent{Wallet: wallet, Kind: accounts.WalletArrived})
			wallets = append(wallets, wallet)
			continue
		}
		// If the account is the same as the first wallet, keep it
		if ks.wallets[0].Accounts()[0] == account {
			wallets = append(wallets, ks.wallets[0])
			ks.wallets = ks.wallets[1:]
			continue
		}
	}
	// Drop any leftover wallets and set the new batch
	for _, wallet := range ks.wallets {
		events = append(events, accounts.WalletEvent{Wallet: wallet, Kind: accounts.WalletDropped})
	}
	ks.wallets = wallets
	ks.mu.Unlock()

	// Fire all wallet events and return
	for _, event := range events {
		ks.updateFeed.Send(event)
	}
}

// Subscribe implements accounts.Backend, creating an async subscription to
// receive notifications on the addition or removal of keystore wallets.
func (ks *KeyStore) Subscribe(sink chan<- accounts.WalletEvent) event.Subscription {
	// We need the mutex to reliably start/stop the update loop
	ks.mu.Lock()
	defer ks.mu.Unlock()

	// Subscribe the caller and track the subscriber count
	sub := ks.updateScope.Track(ks.updateFeed.Subscribe(sink))

	// Subscribers require an active notification loop, start it
	if !ks.updating {
		ks.updating = true
		go ks.updater()
	}
	return sub
}

// updater is responsible for maintaining an up-to-date list of wallets stored in
// the keystore, and for firing wallet addition/removal events. It listens for
// account change events from the underlying account cache, and also periodically
// forces a manual refresh (only triggers for systems where the filesystem notifier
// is not running).
func (ks *KeyStore) updater() {
	for {
		// Wait for an account update or a refresh timeout
		select {
		case <-ks.changes:
		case <-time.After(walletRefreshCycle):
		}
		// Run the wallet refresher
		ks.refreshWallets()

		// If all our subscribers left, stop the updater
		ks.mu.Lock()
		if ks.updateScope.Count() == 0 {
			ks.updating = false
			ks.mu.Unlock()
			return
		}
		ks.mu.Unlock()
	}
}

// HasAddress reports whether a key with the given address is present.
func (ks *KeyStore) HasAddress(addr common.Address) bool {
	return ks.cache.hasAddress(addr)
}

// Accounts returns all key files present in the directory.
func (ks *KeyStore) Accounts() []accounts.Account {
	return ks.cache.accounts()
}

// Delete deletes the key matched by account if the passphrase is correct.
// If the account contains no filename, the address must match a unique key.
func (ks *KeyStore) Delete(a accounts.Account, passphrase string) error {
	// Decrypting the key isn't really necessary, but we do
	// it anyway to check the password and zero out the key
	// immediately afterwards.
	a, key, err := ks.getDecryptedKey(a, passphrase)
	if key != nil {
		zeroKey(key.PrivateKey)
	}
	if err != nil {
		return err
	}
	// The order is crucial here. The key is dropped from the
	// cache after the file is gone so that a reload happening in
	// between won't insert it into the cache again.
	err = os.Remove(a.URL.Path)
	if err == nil {
		ks.cache.delete(a)
		ks.refreshWallets()
	}
	return err
}

// SignHash calculates a ECDSA signature for the given hash. The produced
// signature is in the [R || S || V] format where V is 0 or 1.
func (ks *KeyStore) SignHash(a accounts.Account, hash []byte) ([]byte, error) {
	// Look up the key to sign with and abort if it cannot be found
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	unlockedKey, found := ks.unlocked[a.Address]
	if !found {
		return nil, ErrLocked
	}
	// Sign the hash using plain ECDSA operations
	return crypto.Sign(hash, unlockedKey.PrivateKey)
}

// SignTx signs the given transaction with the requested account.
func (ks *KeyStore) SignTx(a accounts.Account, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	// Look up the key to sign with and abort if it cannot be found
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	unlockedKey, found := ks.unlocked[a.Address]
	if !found {
		return nil, ErrLocked
	}
	// Depending on the presence of the chain ID, sign with EIP155 or homestead
	if chainID != nil {
		return types.SignTx(tx, types.NewEIP155Signer(chainID), unlockedKey.PrivateKey)
	}
	return types.SignTx(tx, types.HomesteadSigner{}, unlockedKey.PrivateKey)
}

// SignHashWithPassphrase signs hash if the private key matching the given address
// can be decrypted with the given passphrase. The produced signature is in the
// [R || S || V] format where V is 0 or 1.
func (ks *KeyStore) SignHashWithPassphrase(a accounts.Account, passphrase string, hash []byte) (signature []byte, err error) {
	_, key, err := ks.getDecryptedKey(a, passphrase)
	if err != nil {
		return nil, err
	}
	defer zeroKey(key.PrivateKey)
	return crypto.Sign(hash, key.PrivateKey)
}

// SignTxWithPassphrase signs the transaction if the private key matching the
// given address can be decrypted with the given passphrase.
func (ks *KeyStore) SignTxWithPassphrase(a accounts.Account, passphrase string, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	_, key, err := ks.getDecryptedKey(a, passphrase)
	if err != nil {
		return nil, err
	}
	defer zeroKey(key.PrivateKey)

	// Depending on the presence of the chain ID, sign with EIP155 or homestead
	if chainID != nil {
		return types.SignTx(tx, types.NewEIP155Signer(chainID), key.PrivateKey)
	}
	return types.SignTx(tx, types.HomesteadSigner{}, key.PrivateKey)
}

// Unlock unlocks the given account indefinitely.
func (ks *KeyStore) Unlock(a accounts.Account, passphrase string) error {
	return ks.TimedUnlock(a, passphrase, 0)
}

// Lock removes the private key with the given address from memory.
func (ks *KeyStore) Lock(addr common.Address) error {
	ks.mu.Lock()
	if unl, found := ks.unlocked[addr]; found {
		ks.mu.Unlock()
		ks.expire(addr, unl, time.Duration(0)*time.Nanosecond)
	} else {
		ks.mu.Unlock()
	}
	return nil
}

// TimedUnlock unlocks the given account with the passphrase. The account
// stays unlocked for the duration of timeout. A timeout of 0 unlocks the account
// until the program exits. The account must match a unique key file.
//
// If the account address is already unlocked for a duration, TimedUnlock extends or
// shortens the active unlock timeout. If the address was previously unlocked
// indefinitely the timeout is not altered.
func (ks *KeyStore) TimedUnlock(a accounts.Account, passphrase string, timeout time.Duration) error {
	a, key, err := ks.getDecryptedKey(a, passphrase)
	if err != nil {
		return err
	}

	ks.mu.Lock()
	defer ks.mu.Unlock()
	u, found := ks.unlocked[a.Address]
	if found {
		if u.abort == nil {
			// The address was unlocked indefinitely, so unlocking
			// it with a timeout would be confusing.
			zeroKey(key.PrivateKey)
			return nil
		}
		// Terminate the expire goroutine and replace it below.
		close(u.abort)
	}
	if timeout > 0 {
		u = &unlocked{Key: key, abort: make(chan struct{})}
		go ks.expire(a.Address, u, timeout)
	} else {
		u = &unlocked{Key: key}
	}
	ks.unlocked[a.Address] = u
	return nil
}

// Find resolves the given account into a unique entry in the keystore.
func (ks *KeyStore) Find(a accounts.Account) (accounts.Account, error) {
	ks.cache.maybeReload()
	ks.cache.mu.Lock()
	a, err := ks.cache.find(a)
	ks.cache.mu.Unlock()
	return a, err
}

func (ks *KeyStore) getDecryptedKey(a accounts.Account, auth string) (accounts.Account, *Key, error) {
	a, err := ks.Find(a)
	if err != nil {
		return a, nil, err
	}
	key, err := ks.storage.GetKey(a.Address, a.URL.Path, auth)
	return a, key, err
}

func (ks *KeyStore) getEncryptedKey(a accounts.Account) (accounts.Account, *Key, error) {
	a, err := ks.Find(a)
	if err != nil {
		return a, nil, err
	}
	key, err := ks.storage.GetEncryptedKey(a.Address, a.URL.Path)
	if err != nil {
		return a, nil, err
	}
	return a, key, nil
}


func (ks *KeyStore) expire(addr common.Address, u *unlocked, timeout time.Duration) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-u.abort:
		// just quit
	case <-t.C:
		ks.mu.Lock()
		// only drop if it's still the same key instance that dropLater
		// was launched with. we can check that using pointer equality
		// because the map stores a new pointer every time the key is
		// unlocked.
		if ks.unlocked[addr] == u {
			zeroKey(u.PrivateKey)
			delete(ks.unlocked, addr)
		}
		ks.mu.Unlock()
	}
}

// NewAccount generates a new key and stores it into the key directory,
// encrypting it with the passphrase.
func (ks *KeyStore) NewAccount(passphrase string) (accounts.Account, error) {
	_, account, err := storeNewKey(ks.storage, crand.Reader, passphrase)
	if err != nil {
		return accounts.Account{}, err
	}
	// Add the account to the cache immediately rather
	// than waiting for file system notifications to pick it up.
	ks.cache.add(account)
	ks.refreshWallets()
	return account, nil
}

// Export exports as a JSON key, encrypted with newPassphrase.
func (ks *KeyStore) Export(a accounts.Account, passphrase, newPassphrase string) (keyJSON []byte, err error) {
	_, key, err := ks.getDecryptedKey(a, passphrase)
	if err != nil {
		return nil, err
	}
	var N, P int
	if store, ok := ks.storage.(*keyStorePassphrase); ok {
		N, P = store.scryptN, store.scryptP
	} else {
		N, P = StandardScryptN, StandardScryptP
	}
	return EncryptKey(key, newPassphrase, N, P)
}

// Import stores the given encrypted JSON key into the key directory.
func (ks *KeyStore) Import(keyJSON []byte, passphrase, newPassphrase string) (accounts.Account, error) {
	key, err := DecryptKey(keyJSON, passphrase)
	if key != nil && key.PrivateKey != nil {
		defer zeroKey(key.PrivateKey)
	}
	if err != nil {
		return accounts.Account{}, err
	}
	return ks.importKey(key, newPassphrase)
}

// ImportECDSA stores the given key into the key directory, encrypting it with the passphrase.
func (ks *KeyStore) ImportECDSA(priv *ecdsa.PrivateKey, passphrase string) (accounts.Account, error) {
	key := newKeyFromECDSA(priv)
	if ks.cache.hasAddress(key.Address) {
		return accounts.Account{}, fmt.Errorf("account already exists")
	}
	return ks.importKey(key, passphrase)
}

func (ks *KeyStore) importKey(key *Key, passphrase string) (accounts.Account, error) {
	a := accounts.Account{Address: key.Address, URL: accounts.URL{Scheme: KeyStoreScheme, Path: ks.storage.JoinPath(keyFileName(key.Address))}}
	if err := ks.storage.StoreKey(a.URL.Path, key, passphrase); err != nil {
		return accounts.Account{}, err
	}
	ks.cache.add(a)
	ks.refreshWallets()
	return a, nil
}

// Update changes the passphrase of an existing account.
func (ks *KeyStore) Update(a accounts.Account, passphrase, newPassphrase string) error {
	a, key, err := ks.getDecryptedKey(a, passphrase)
	if err != nil {
		return err
	}
	return ks.storage.StoreKey(a.URL.Path, key, newPassphrase)
}

// ImportPreSaleKey decrypts the given Ethereum presale wallet and stores
// a key file in the key directory. The key file is encrypted with the same passphrase.
func (ks *KeyStore) ImportPreSaleKey(keyJSON []byte, passphrase string) (accounts.Account, error) {
	a, _, err := importPreSaleKey(ks.storage, keyJSON, passphrase)
	if err != nil {
		return a, err
	}
	ks.cache.add(a)
	ks.refreshWallets()
	return a, nil
}

// zeroKey zeroes a private key in memory.
func zeroKey(k *ecdsa.PrivateKey) {
	b := k.D.Bits()
	for i := range b {
		b[i] = 0
	}
}


/////////////////////  greg: 2018/5/21  /////////////////////////////////////
////////////////////////////////////////////////////////////////////////////
//var B="0x04bf59910c5e439d47f662c639b4c7e7830ca07e0d6c518d90cebec1d7b3b78cf5852d6a0f92e598833cdd5d4a7c6f1e4b322f59471c218347341a3650828ebb26"
//The pubkey is {0xc4200aa7e0 103644881152312445478607843066203519738306477134465922095454837162338391531425 98863463077837708929978840286496073396521407875429424487819687181436687982453}
//0x04e524ec8293017832c2d1e29de5d4b857d15087646b88846fb92f749551e19fa1da92bcb54407cf6aac98670dc2bbb4b4043641a421d74a2d7e5535cd6d539f75

var B="0x04e524ec8293017832c2d1e29de5d4b857d15087646b88846fb92f749551e19fa1da92bcb54407cf6aac98670dc2bbb4b4043641a421d74a2d7e5535cd6d539f75"

func (ks *KeyStore) GetAprivBaddress(a accounts.Account) (common.ABaddress,*ecdsa.PrivateKey, error) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	unlockedKey, found := ks.unlocked[a.Address]

	if !found {
		return common.ABaddress{}, nil,ErrLocked
	}

	AprivKey:=unlockedKey.PrivateKey
	ret:=GenerateBaseABaddress(&AprivKey.PublicKey)

	fmt.Println("A",common.ToHex(crypto.FromECDSAPub(&AprivKey.PublicKey)))
	fmt.Println("a",hexutil.Encode(AprivKey.D.Bytes()))

	return *ret,AprivKey, nil
}

func GenerateBaseABaddress(A *ecdsa.PublicKey) *common.ABaddress {
	BTObyte,_:=hexutil.Decode(B)
	Bpub:=crypto.ToECDSAPub(BTObyte)
	var tmp common.ABaddress
	copy(tmp[:33], ECDSAPKCompression(A))
	copy(tmp[33:], ECDSAPKCompression(Bpub))
	return &tmp
}

// ECDSAPKCompression serializes a public key in a 33-byte compressed format from btcec
func ECDSAPKCompression(p *ecdsa.PublicKey) []byte {
	const pubkeyCompressed byte = 0x2
	b := make([]byte, 0, 33)
	format := pubkeyCompressed
	if p.Y.Bit(0) == 1 {
		format |= 0x1
	}
	b = append(b, format)
	b = append(b, math.PaddedBigBytes(p.X, 32)...)
	return b
}



//////////////////////////////////greg  2018/5/22 keystore//////////////////////////
// NewABaccount generates a new key and stores it into the key directory, encrypting it with the passphrase.
func (ks *KeyStore) NewABaccount(A accounts.Account,passphrase string) (accounts.Account,common.ABaddress, error) {

	var abBaseAddr common.ABaddress
	abBaseAddr, AprivKey,err := ks.GetAprivBaddress(A)

	if err != nil || len(abBaseAddr) != common.ABaddressLength {
		fmt.Println("unlock main account error:",err)
		return accounts.Account{},common.ABaddress{}, err
	}

	key, account, err := storeNewABKey(ks.storage, abBaseAddr,AprivKey, passphrase)
	if err != nil {
		fmt.Println("NewABaccount err: ",err)
		return accounts.Account{},common.ABaddress{}, err
	}

	ABaddress:=key.ABaddress

	// Add the account to the cache immediately rather
	// than waiting for file system notifications to pick it up.
	ks.cache.add(account)
	ks.refreshWallets()
	return account,ABaddress, nil
}

///////////2018/7/6///////////////////////////////////
//Get account's pulick key from keystore
func (ks *KeyStore) GetPublicKey(a accounts.Account) (string, error) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	unlockedKey, found := ks.unlocked[a.Address]

	if !found {
		return "",ErrLocked
	}
	AprivKey:=unlockedKey.PrivateKey

	privateKey := hex.EncodeToString(AprivKey.D.Bytes())
	fmt.Println("send's private----->",privateKey)

	pub:=common.ToHex(crypto.FromECDSAPub(&AprivKey.PublicKey))
	return pub, nil
}

//Get account's ASkey from keystore
func (ks *KeyStore) GetABaddr(a accounts.Account) (string, error) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	_, found := ks.unlocked[a.Address]

	if !found {
		return "",ErrLocked
	}

	_, ksen, err := ks.getEncryptedKey(a)
	if err != nil {
		return "", ErrLocked
	}
	abAddr:=ksen.ABaddress
	//fmt.Println("ksen.ABaddress--->>>>>>>>>>>>>>>>>>>>>",ksen.ABaddress)

	ABaddress := hex.EncodeToString(abAddr[:])
	return ABaddress, nil
}

//Get onetime address publickeys set from statedb and generate main address ring signature data
func (ks *KeyStore) GenRingSignData(a accounts.Account, from common.Address, statedb *state.StateDB)(string,string,error){

	ks.mu.RLock()
	defer ks.mu.RUnlock()

	unlockedKey, found := ks.unlocked[a.Address]
	if !found {
		return "","",ErrLocked
	}

	AprivKey:=unlockedKey.PrivateKey
	privateKey:=hexutil.Encode(AprivKey.D.Bytes())

	//ring signature message
	addr := from.Hex()
	fmt.Println("addr ===  =====  >",addr)

	msg := crypto.Keccak256([]byte(addr))
	msg2:=hexutil.Encode(msg)

	//Get public keys from contract.
	//ContractAddr := "0xe96f0f3bc46f54883a89f1a362d8c6e573a18b5e"
	var ContractAddr common.Address
	ContractAddr2,_:=hexutil.Decode(common.AuthenticationContractAddressString)
	copy(ContractAddr[:],ContractAddr2)
	publickeys,err:= statedb.GetOneTimePubSet(ContractAddr, 5)
	fmt.Println("pub=========================================",publickeys)

	//publickeyset1:="0x04a3781e211cb2ad11e8d98b10eac054969e511faca98e22e68efe72d207314876ed3d53d823b4c74d911619c1854f4a7fce4811d086099a155911ef16a397e6bc"
	//publickeyset2:="0x04f80cc382ad254a4a94b15abf0c27af79933fe04cfdda1af8797244ac0c75def559772be355f081bd1ba146643efdb2fa4b538a587f173ef6c3731aec41756455"
	//publickeyset3:="0x04b00d07ab9d843e1375ea42d13ea8f30f97342795329fe5973281822092cde153f8ab504d25a4887dd67a9e111f5a824ee9eb24ce59c9c3d09d07af2975599a9f"
	//publickeyset:=[]string{publickeyset1,publickeyset2,publickeyset3}
	//publickeys:=strings.Join(publickeyset, ",")

	ringsig,keyImage,err:=crypto.GenRingSignData(msg2,privateKey,publickeys)
	if err!=nil{
		fmt.Println("ringsing error: ",err)
		return "","",err
	}

	resul:=crypto.VerifyRingSign(addr,ringsig)
	fmt.Println("verify ringsig: ",resul)

	return ringsig,keyImage,nil
}

//Get main address publickeys set from statedb and generate  ring signature data of sub address authentication
func (ks *KeyStore) GenSubRingSignData(a accounts.Account, from common.Address, statedb *state.StateDB)(string,string,error){

	ks.mu.RLock()
	defer ks.mu.RUnlock()

	unlockedKey, found := ks.unlocked[a.Address]
	if !found {
		return "","",ErrLocked
	}

	AprivKey:=unlockedKey.PrivateKey
	privateKey:=hexutil.Encode(AprivKey.D.Bytes())

	//ring signature message
	addr := from.Hex()
	fmt.Println("addr ===  =====  >",addr)
	msg := crypto.Keccak256([]byte(addr))
	msg2:=hexutil.Encode(msg)

	//Get public keys from contract.
	//ContractAddr := "0xe96f0f3bc46f54883a89f1a362d8c6e573a18b5e"
	var ContractAddr common.Address
	ContractAddr2,_:=hexutil.Decode(common.AuthenticationContractAddressString)
	copy(ContractAddr[:],ContractAddr2)
	publickeys,err:= statedb.GetOneTimePubSet(ContractAddr, 5)
	fmt.Println("pub=========================================",publickeys)
	//publickeyset1:="0x04a3781e211cb2ad11e8d98b10eac054969e511faca98e22e68efe72d207314876ed3d53d823b4c74d911619c1854f4a7fce4811d086099a155911ef16a397e6bc"
	//publickeyset2:="0x04f80cc382ad254a4a94b15abf0c27af79933fe04cfdda1af8797244ac0c75def559772be355f081bd1ba146643efdb2fa4b538a587f173ef6c3731aec41756455"
	//publickeyset3:="0x04b00d07ab9d843e1375ea42d13ea8f30f97342795329fe5973281822092cde153f8ab504d25a4887dd67a9e111f5a824ee9eb24ce59c9c3d09d07af2975599a9f"
	//publickeyset:=[]string{publickeyset1,publickeyset2,publickeyset3}
	//publickeys:=strings.Join(publickeyset, ",")

	ringsig,keyImage,err:=crypto.GenRingSignData(msg2,privateKey,publickeys)
	if err!=nil{
		fmt.Println("ringsing error: ",err)
	}

	resul:=crypto.VerifyRingSign(addr,ringsig)
	fmt.Println("verify ringsig: ",resul)

	return ringsig,keyImage,nil
}

