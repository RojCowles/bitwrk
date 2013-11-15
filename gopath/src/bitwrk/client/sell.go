//  BitWrk - A Bitcoin-friendly, anonymous marketplace for computing power
//  Copyright (C) 2013  Jonas Eschenburg <jonas@bitwrk.net>
//
//  This program is free software: you can redistribute it and/or modify
//  it under the terms of the GNU General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  This program is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU General Public License for more details.
//
//  You should have received a copy of the GNU General Public License
//  along with this program.  If not, see <http://www.gnu.org/licenses/>.

package client

import (
	"bitwrk"
	"bitwrk/bitcoin"
	"bitwrk/cafs"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"sync"
	"time"
)

type SellActivity struct {
	Trade

	workerInfo WorkerInfo
}

func (m *ActivityManager) NewSell(info *WorkerInfo) (*SellActivity, error) {
	now := time.Now()

	result := &SellActivity{
		Trade: Trade{
			condition:    sync.NewCond(new(sync.Mutex)),
			manager:      m,
			key:          m.NewKey(),
			started:      now,
			lastUpdate:   now,
			bidType:      bitwrk.Sell,
			article:      info.Article,
			encResultKey: new(bitwrk.Tkey),
		},
		workerInfo: *info,
	}

	// Get a random key for encrypting the result
	if _, err := rand.Reader.Read(result.encResultKey[:]); err != nil {
		return nil, err
	}

	m.register(result.key, result)
	return result, nil
}

func (a *SellActivity) Perform(receiveManager *ReceiveManager) error {
	defer a.manager.unregister(a.key)
	// wait for grant or reject
	log.Println("Waiting for permission")

	// Get a permission for the sell
	if err := a.awaitPermission(); err != nil {
		return err
	}
	log.Printf("Got permission. Price: %v", a.price)

	endpoint := receiveManager.NewEndpoint()
	defer endpoint.Dispose()

	if err := a.awaitBid(); err != nil {
		return err
	}
	log.Printf("Got bid id: %v", a.bidId)

	if err := a.awaitTransaction(); err != nil {
		return err
	}
	log.Printf("Got transaction id: %v", a.txId)

	if tx, etag, err := FetchTx(a.txId, ""); err != nil {
		return err
	} else {
		a.tx = tx
		a.txETag = etag
		log.Printf("Tx-etag: %#v", etag)
	}

	// TODO: Verify the transaction

	// Start polling for state changes in background
	go func() {
		a.pollTransaction()
	}()

	var backchannel *backchannel
	if b, err := a.receiveWorkData(endpoint); err != nil {
		return err
	} else {
		backchannel = b
	}

	log.Println("Got work data. Publishing buyer's secret.")
	if err := SendTxMessagePublishBuyerSecret(a.txId, a.identity, a.buyerSecret); err != nil {
		backchannel.release <- 0
		return err
	}

	log.Println("Awaiting receipt...")
	endpoint.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		a.handleReceipt(w, r)
	})

	if err := a.dispatchWorkAndSaveEncryptedResult(backchannel.workFile); err != nil {
		backchannel.release <- 0
		return err
	}

	// Getting the result has possibly taken too long
	a.condition.L.Lock()
	state := a.tx.State
	a.condition.L.Unlock()
	if state != bitwrk.StateActive {
		backchannel.release <- 0
		return errors.New("Transaction expired before sending back encrypted result")
	}

	if err := a.transmitEncryptedResultBackToBuyer(backchannel.w); err != nil {
		backchannel.release <- 0
		return err
	}

	backchannel.release <- 0

	a.waitForTransactionPhase(bitwrk.PhaseFinished, bitwrk.PhaseWorking, bitwrk.PhaseUnverified)
	return nil
}

func (a *SellActivity) dispatchWorkAndSaveEncryptedResult(workFile cafs.File) error {
	reader := workFile.Open()
	defer reader.Close()

	resp, err := client.Post(a.workerInfo.PushURL, "application/octet-stream", reader)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Worker returned status: %v", resp.Status)
	}

	temp := a.manager.storage.Create()
	defer temp.Dispose()

	// Use AES-256 to encrypt the result
	block, err := aes.NewCipher(a.encResultKey[:])
	if err != nil {
		return err
	}

	// Create OFB stream with null initialization vector (ok for one-time key)
	var iv [aes.BlockSize]byte
	stream := cipher.NewOFB(block, iv[:])

	writer := &cipher.StreamWriter{S: stream, W: temp}
	_, err = io.Copy(writer, resp.Body)
	if err != nil {
		return err
	}

	if err := temp.Close(); err != nil {
		return err
	}

	if err := resp.Body.Close(); err != nil {
		return err
	}

	if file, err := temp.File(); err != nil {
		return err
	} else {
		a.encResultFile = file
	}

	return nil
}

func (a *SellActivity) transmitEncryptedResultBackToBuyer(writer io.Writer) error {
	reader := a.encResultFile.Open()
	defer reader.Close()

	if _, err := io.Copy(writer, reader); err != nil {
		return err
	}

	return nil
}

type backchannel struct {
	ready    bool
	workFile cafs.File
	w        http.ResponseWriter
	release  chan int
}

// Blocks until work data has been received. Returns a back channel to the
// buyer. The back channel must be released by the caller.
func (a *SellActivity) receiveWorkData(endpoint *Endpoint) (*backchannel, error) {
	result := backchannel{
		ready:   false,
		release: make(chan int),
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		a.handleRequest(w, r, &result)
	}

	endpoint.SetHandler(handler)

	log.Printf("Listening on %v", endpoint.URL())
	if err := SendTxMessageEstablishSeller(a.txId, a.identity, endpoint.URL()); err != nil {
		return nil, err
	}

	var ready, active bool
	a.waitWhile(func() bool {
		ready = result.ready
		active = a.Trade.tx.State == bitwrk.StateActive
		return !ready && active
	})

	if !ready {
		return nil, errors.New("Transaction no longer active")
	}

	if !active {
		log.Printf("Got backchannel too late")
		result.release <- 0
		return nil, errors.New("Transaction no longer active")
	}

	return &result, nil
}

// Handles the incoming work request, up to the point where the work package
// has been identified as legit. Then, the controlling goroutine takes over
// while the request handler waits for the back-channel to be released.
func (a *SellActivity) handleRequest(w http.ResponseWriter, r *http.Request, backchannel *backchannel) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var mreader *multipart.Reader
	if _reader, err := r.MultipartReader(); err != nil {
		log.Printf("Error parsing multipart content: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else {
		mreader = _reader
	}

	temp := a.manager.GetStorage().Create()
	defer temp.Dispose()

	var workFile cafs.File
	var buyersecret bitwrk.Thash
	gotBuyerSecret := false

	// iterate through parts of multipart/form-data content
	for {
		part, err := mreader.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			log.Printf("Error reading part: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		formName := part.FormName()
		switch formName {
		case "buyersecret":
			b := make([]byte, 64)
			n, err := part.Read(b)
			if err != nil || n != len(b) {
				log.Printf("Error reading buyersecret: %v (%v bytes read)", err, n)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			n, err = hex.Decode(buyersecret[:], b)
			if err != nil || n != len(buyersecret) {
				log.Printf("Error decoding buyersecret: %v (%v bytes written)", err, n)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			gotBuyerSecret = true
		case "work":
			const MAXBYTES = 2 << 24 // 16MB
			// Copy up to MAXBYTES and expect EOF
			if n, err := io.CopyN(temp, part, MAXBYTES); err != io.EOF {
				log.Printf("Work too long or error: %v (%v bytes read)", err, n)
				http.Error(w, "Error handling work", http.StatusBadRequest)
				return
			}
			temp.Close()
			if file, err := temp.File(); err != nil {
				log.Printf("Error creating file from temporary data: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else {
				workFile = file
			}
		default:
			log.Printf("Don't know what to do with part %#v", formName)
			http.Error(w, "Unknown part", http.StatusBadRequest)
			return
		}
	}

	if workFile == nil || !gotBuyerSecret {
		log.Printf("Incomplete work message. Got buyer secret: %v", gotBuyerSecret)
		http.Error(w, "Incomplete work message", http.StatusBadRequest)
		return
	}

	active := true
	var workHash, workSecretHash *bitwrk.Thash
	log.Printf("Watching transaction state...")
	a.waitWhile(func() bool {
		active = a.tx.State == bitwrk.StateActive
		workHash, workSecretHash = a.tx.WorkHash, a.tx.WorkSecretHash
		log.Printf("  state: %v    phase: %v", a.tx.State, a.tx.Phase)
		return active && a.tx.Phase != bitwrk.PhaseTransmitting
	})

	if !active {
		log.Printf("Transaction timed out waiting for buyer to establish")
		http.Error(w, "Transaction timeout", http.StatusInternalServerError)
		return
	}

	if *workHash != bitwrk.Thash(workFile.Key()) {
		log.Printf("WorkHash and received data do not match")
		http.Error(w, "WorkHash and received data do not match", http.StatusBadRequest)
		return
	}

	if err := verifyBuyerSecret(workHash, workSecretHash, &buyersecret); err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Looks like this is a valid work package. Prepare back channel.
	a.Trade.condition.L.Lock()
	if backchannel.ready {
		a.Trade.condition.L.Unlock()
		log.Println("Backchannel is already consumed.")
		http.Error(w, "Backchannel is already consumed.", http.StatusInternalServerError)
		return
	}
	if a.Trade.tx.State != bitwrk.StateActive {
		a.Trade.condition.L.Unlock()
		log.Println("Transaction no longer active.")
		http.Error(w, "Transaction no longer active.", http.StatusInternalServerError)
		return
	}
	backchannel.ready = true
	backchannel.workFile = workFile
	backchannel.w = w
	a.Trade.buyerSecret = &buyersecret
	a.Trade.condition.Broadcast()
	a.Trade.condition.L.Unlock()

	// Let the main goroutine do the job
	<-backchannel.release
}

func (a *SellActivity) handleReceipt(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	encresulthash := r.FormValue("encresulthash")
	if encresulthash != a.encResultFile.Key().String() {
		http.Error(w, "Encoded result hash is wrong", http.StatusBadRequest)
		return
	}

	sig := r.FormValue("encresulthashsig")
	if err := bitcoin.VerifySignatureBase64(encresulthash, a.tx.Buyer, sig); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	encresultkey := hex.EncodeToString(a.encResultKey[:])

	log.Println("Got valid receipt for result. Signaling transmit finished.")

	if err := SendTxMessageTransmitFinished(a.txId, a.identity,
		encresulthash, sig, encresultkey); err != nil {
		log.Printf("Signaling transmit finished failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	a.condition.L.Lock()
	a.encResultHashSig = sig
	a.condition.Broadcast()
	a.condition.L.Unlock()
}

func verifyBuyerSecret(workHash, workSecretHash, buyerSecret *bitwrk.Thash) error {
	sha := sha256.New()
	sha.Write(workHash[:])
	sha.Write(buyerSecret[:])
	var result bitwrk.Thash
	sha.Sum(result[:0])
	if result != *workSecretHash {
		return errors.New("Buyer's secret does not match work hash and work secret hash")
	}
	return nil
}
