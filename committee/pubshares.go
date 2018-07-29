// Copyright 2018 The go-usechain Authors
// This file is part of the go-usechain library.
//
// The go-usechain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-usechain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-usechain library. If not, see <http://www.gnu.org/licenses/>.

package committee

import (
	"fmt"
	"github.com/usechain/go-usechain/accounts"
	"github.com/usechain/go-usechain/accounts/keystore"
	"github.com/usechain/go-usechain/commitee/sssa"
	"github.com/usechain/go-usechain/common"
	"github.com/usechain/go-usechain/common/hexutil"
	"github.com/usechain/go-usechain/core/state"
	"github.com/usechain/go-usechain/crypto"
	"github.com/usechain/go-usechain/eth"
	"github.com/usechain/go-usechain/log"
	"github.com/usechain/go-usechain/core/types"
	"crypto/ecdsa"
	"math/big"
	"strconv"
	"errors"
	"github.com/usechain/go-usechain/internal/ethapi"
	"encoding/hex"
	"bytes"
	"github.com/usechain/go-usechain/cmd/utils"
)

/*
 * Each commitee get own share t_i, and
 * return t_1 * A
 */
func GeneratePubShare(pubSet []*ecdsa.PublicKey) string {
	//privateShares := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE=Uv8TKu9w935MhVhKudhksXv1QQO_KijTVQ5yCWQNaL4="
	privateShares := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAI=dwOoQA6zD-kc0KQHm7srZ7sePn_pkOIalCZGbTD1WrI="

	ID := privateShares[:44]
	Shares := sssa.FromBase64(privateShares[44:])

	var sharePubSet []ecdsa.PublicKey = make([]ecdsa.PublicKey, len(pubSet))
	sharePubStr := ""

	for i := range pubSet {
		sharePubSet[i].Curve = crypto.S256()
		sharePubSet[i].X, sharePubSet[i].Y = crypto.S256().ScalarMult(pubSet[i].X, pubSet[i].Y, Shares.Bytes())

		fmt.Println("::::::::::::::::privateShares", pubSet[i])

		sharePubStr = sharePubStr + ID + sssa.ToBase64(sharePubSet[i].X) + sssa.ToBase64(sharePubSet[i].Y)

	}

	sharePubStr = sssa.FormatData44bytes(strconv.Itoa(len(pubSet))) + sharePubStr
	fmt.Println("sharePubStr:", sharePubStr)
	return sharePubStr
}


/*
 *  Extract the pubShareMsg
 *  The PubSharesMsg format
 *  { A1S1: 132 bytes  certID: 44 bytes  senderID: 44 bytes  pubNum: 44 bytes pubArray :[ ID : 44bytes pub.X: 44 bytes pub.Y: 44 bytes] }
 *  return the A1S1, certID, senderID, pubNum, pubArray
 */
func ExtractPubShareMsg(msg string) (string, int, int, string, error){
	if len(msg) < 266 + 132 {
		return "", 0, 0, "", errors.New("pub share msg gota invalided length")
	}

	A1S1 := msg[2:134]
	certID, err := strconv.Atoi(msg[134:178])
	if err != nil {
		return "", 0, 0, "", errors.New("pub shares msg format error")
	}

	senderID, err := strconv.Atoi(msg[178:222])
	if err != nil {
		return "", 0, 0, "", errors.New("pub shares msg format error")
	}

	pubSharesNum, err := strconv.Atoi(msg[222:266])
	if err != nil {
		return "", 0, 0, "", errors.New("pub shares msg format error")
	}

	log.Debug("pubSharesNum", pubSharesNum)
	if err != nil || len(msg) < 266 + 132 * pubSharesNum {
		return "", 0, 0, "", errors.New("pub shares msg format error")
	}

	shares := msg[266:]
	return A1S1, certID, senderID, shares, nil
}

/*
 * Extract pubshares into pubkey array
 * Return checking stat & the pubkey array
 */
func extractPubshare(pubShares string) (bool, []string){
	if len(pubShares) % 132 != 0 {
		return false, nil
	}

	shareNum := len(pubShares) / 132
	var shares []string = make([]string, shareNum)
	for i := 0; i < shareNum; i++ {
		shares[i] = pubShares[(0 + 132 * i) : (132 + 132 * i)]
	}
	return true, shares
}


/*
 *  Simple history verify msg storage & check
 */
///TODO: update the data storage
var MsgMap = make(map[string][]string)
var MsgCheckMap = make(map[string]([]int))

func InStringArraySet(a1s1 string, senderId int) bool{
	if _, ok := MsgCheckMap[a1s1]; ok {
		for i := range MsgCheckMap[a1s1] {
			if i == senderId && MsgCheckMap[a1s1][i] == 1{
				return true
			}
		}
	}
	return false
}

/*
 *  Check the subAccount whether get a matched main account
 *  Return the match stat
 */
///TODO:update late for intelligent select
func CheckGetValidA1S1(a1s1 string) bool {
	sbyte,_:=hexutil.Decode("0x" + a1s1)
	A1, S1, err := keystore.GeneratePKPairFromABaddress(sbyte[:])
	if err !=nil {
		log.Error("A1S1 decode failed!", err)
		return false
	}

	//scan the main account, to find whether get a matched account
	var tmpSet []string = make([]string, 2)
	for i := range MsgMap[a1s1] {
		for j := range MsgMap[a1s1] {
			if i < j {
				err, pubSet01 := extractPubshare(MsgMap[a1s1][i])
				if err == false {
					return false
				}

				err, pubSet02 := extractPubshare(MsgMap[a1s1][j])
				if err == false {
					return false
				}

				for m := range pubSet01 {
					for n := range pubSet02 {
						tmpSet[0] = pubSet01[m]
						tmpSet[1] = pubSet02[n]

						//fmt.Println("tmp:", tmpSet)
						combined, err := sssa.CombineECDSAPubs(tmpSet)
						if err != nil {
							log.Debug("Fatal: combining: ", err)
							continue
						}
						bA := crypto.ToECDSAPub([]byte(combined))
						A1Check := crypto.ScanPubSharesA1(bA, S1)

						if A1.X.Cmp(A1Check.X) == 0 && A1.Y.Cmp(A1Check.Y) == 0 {
							log.Debug("Get a matched account!")
							return true
						}
					}
				}
			}
		}
	}
	log.Debug("Failed to get a matched account")
	return false
}

/*
 *  Committee send msg through tx, return the send stat
 *  Return the tx sending stat
 */
func SendCommitteeMsg(ethereum *eth.Ethereum, msg string) bool {
	// Look up the wallet containing the requested signer
	coinbase, err := ethereum.Etherbase()
	if err != nil {
		log.Error("Be a committee must ","err", err)
		return false
	}
	account := accounts.Account{Address: coinbase}

	fmt.Println("coinbase are:", coinbase)
	wallet, err := ethereum.AccountManager().Find(account)
	if err != nil {
		log.Error("To be a committee of usechain, need local account","err", err)
		return false
	}

	//new a transaction, sign it & add to tx pool
	pendingStat := ethereum.TxPool().State()
	msgEncrypted := []byte(*ethapi.SendMsgWithTag([]byte(msg)))
	tx := types.NewTransaction(pendingStat.GetNonce(coinbase), common.HexToAddress(OneVerifierAddress), nil, 60000000, big.NewInt(20000000000), msgEncrypted)
	signedTx, err := wallet.SignTxWithPassphrase(account, "123456", tx, ethereum.ChainID())
	if err != nil {
		utils.Fatalf("Please ensure the coinbase account got the passphrase with \"123456\", sign the committee Msg failed :", err)
	}
	ethereum.TxPool().AddLocal(signedTx)

	log.Info("Submitted transaction", "fullhash", signedTx.Hash().Hex(), "recipient", tx.To())
	return true
}


/*
 * After verified the account, send a confirm tx to authentication contract
 * Return the tx sending stat
 */
func SendAccountConfirmMsg(ethereum *eth.Ethereum, certID int, confirmStat int) bool {
	// Look up the wallet containing the requested signer
	coinbase, err := ethereum.Etherbase()
	if err != nil {
		log.Error("Be a committee must ","err", err)
		return false
	}
	account := accounts.Account{Address: coinbase}
	wallet, err := ethereum.AccountManager().Find(account)
	if err != nil {
		log.Error("To be a committee of usechain, need local account","err", err)
		return false
	}

	msgStr := "0xc03c1796" + state.FormatData64bytes(strconv.Itoa(certID)) + state.FormatData64bytes(strconv.Itoa(confirmStat))
	msg, err := hexutil.Decode(msgStr)

	//new a transaction
	pendingStat := ethereum.TxPool().State()
	tx := types.NewTransaction(pendingStat.GetNonce(coinbase), common.HexToAddress(common.AuthenticationContractAddressString), nil, 60000000, nil, msg)
	signedTx, err := wallet.SignTxWithPassphrase(account, "123456", tx, ethereum.ChainID())
	if err != nil {
		log.Error("Sign the committee Msg failed :", err)
	}
	ethereum.TxPool().AddLocal(signedTx)

	log.Info("Submitted transaction", "fullhash", signedTx.Hash().Hex(), "recipient", tx.To())
	return true
}


/*
 * Read the uncomfirmAddresses from the authentication contract
 * Return the certID, ringSig, pubSkey, checkCertID
 */
func ReadUnconfirmedAddress(usechain *eth.Ethereum, index int64, contractAddr common.Address, checkCertID int64) (string, string, string, int64){
	// generate i's keyindex to check unconfirmed address index
	keyIndex, _ := state.ExpandToIndex(state.UnConfirmedAddress, "", index)
	resultUnConfirmedAddressIndex := usechain.TxPool().State().GetState(contractAddr, common.HexToHash(keyIndex))
	unConfirmedAddressIndex := state.GetLen(resultUnConfirmedAddressIndex[:])
	//fmt.Println("unconfirmed address index: %x\n", resultUnConfirmedAddressIndex.String())

	// check added
	if  checkCertID >= unConfirmedAddressIndex {
		return resultUnConfirmedAddressIndex.String(),"","", 0
	}

	// generate unConfirmedAddress indexed key
	newKeyIndex, _ := state.ExpandToIndex(state.CertToAddress, hex.EncodeToString(resultUnConfirmedAddressIndex[:]), 0)
	resultUnConfirmedAddress := usechain.TxPool().State().GetState(contractAddr, common.HexToHash(newKeyIndex))
	resultUnConfirmedAddr := hex.EncodeToString(resultUnConfirmedAddress[:])
	//fmt.Println("resultUnConfirmedAddress: ", "00"+resultUnConfirmedAddr[:len(resultUnConfirmedAddr)-2])

	// ++++++++++++++++++++++++++++++++++++++++++++
	// get ringSig
	resultRingSig, _ := state.ExpandToIndex(state.CertificateAddr, "00"+resultUnConfirmedAddr[:len(resultUnConfirmedAddr)-2], 1)
	addressRingSig := usechain.TxPool().State().GetState(contractAddr, common.HexToHash(resultRingSig))
	addressRingSigLen := state.GetLen(addressRingSig[:])
	forLen := addressRingSigLen / (int64(common.HashLength) * 2)
	// init query data hash
	var buff bytes.Buffer
	res := ""
	for j := int64(0); j <= forLen; j++ {
		newKeyIndexHash := state.CalculateStateDbIndex(resultRingSig, "")
		newKeyIndexString := state.IncreaseHexByNum(newKeyIndexHash, j)
		result := usechain.TxPool().State().GetState(contractAddr, common.HexToHash(newKeyIndexString))
		buff.Write(result[:])
	}
	res += buff.String()[:addressRingSigLen/2]
	//fmt.Println("addressRingSig: ", res)

	// ++++++++++++++++++++++++++++++++++++++++++++
	// get pubSkey
	resultPubSKey, _ := state.ExpandToIndex(state.CertificateAddr, "00"+resultUnConfirmedAddr[:len(resultUnConfirmedAddr)-2], 2)
	addressPubSKey := usechain.TxPool().State().GetState(contractAddr, common.HexToHash(resultPubSKey))

	addressPubSKeyLen := state.GetLen(addressPubSKey[:])
	forLen1 := addressPubSKeyLen / (int64(common.HashLength) * 2)
	var buff1 bytes.Buffer
	res1 := ""
	for j := int64(0); j <= forLen1; j++ {
		newKeyIndexHash := state.CalculateStateDbIndex(resultPubSKey, "")
		newKeyIndexString := state.IncreaseHexByNum(newKeyIndexHash, j)
		result := usechain.TxPool().State().GetState(contractAddr, common.HexToHash(newKeyIndexString))
		buff1.Write(result[:])
	}
	res1 += buff1.String()[:addressPubSKeyLen/2]
	//fmt.Println("addressPubSKey: ", res1)
	checkCertID = unConfirmedAddressIndex
	return resultUnConfirmedAddressIndex.String(), res, res1, checkCertID
}


