// Copyright 2018 Vector Creations Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package workers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/matrix-org/dendrite/appservice/storage"
	"github.com/matrix-org/dendrite/appservice/types"
	"github.com/matrix-org/dendrite/common/config"
	"github.com/matrix-org/gomatrixserverlib"
	log "github.com/sirupsen/logrus"
)

var (
	// Maximum size of events sent in each transaction.
	// Warning, if this is lowered and a number of events greater than the previous
	// batch size were still to be sent, then a number of events equal to the
	// difference will be ignored by the app service.
	// TL;DR: Don't lower this number with any AS events still left in the database.
	transactionBatchSize = 50
	// Timeout for sending a single transaction to an application service.
	transactionTimeout = time.Second * 60
	// The current transaction ID. Increments after every successful transaction.
	currentTransactionID = 0
)

// SetupTransactionWorkers spawns a separate goroutine for each application
// service. Each of these "workers" handle taking all events intended for their
// app service, batch them up into a single transaction (up to a max transaction
// size), then send that off to the AS's /transactions/{txnID} endpoint. It also
// handles exponentially backing off in case the AS isn't currently available.
func SetupTransactionWorkers(
	appserviceDB *storage.Database,
	workerStates []types.ApplicationServiceWorkerState,
) error {
	// Create a worker that handles transmitting events to a single homeserver
	for _, workerState := range workerStates {
		// Don't create a worker if this AS doesn't want to receive events
		if workerState.AppService.URL != "" {
			go worker(appserviceDB, workerState)
		}
	}
	return nil
}

// worker is a goroutine that sends any queued events to the application service
// it is given.
func worker(db *storage.Database, ws types.ApplicationServiceWorkerState) {
	log.WithFields(log.Fields{
		"appservice": ws.AppService.ID,
	}).Info("starting application service")
	ctx := context.Background()

	// Initialize transaction ID counter
	var err error
	currentTransactionID, err = db.GetTxnIDWithAppServiceID(ctx, ws.AppService.ID)
	if err != nil && err != sql.ErrNoRows {
		log.WithFields(log.Fields{
			"appservice": ws.AppService.ID,
		}).WithError(err).Fatal("appservice worker unable to get latest transaction ID from DB")
		return
	}

	// Grab the HTTP client for sending requests to app services
	client := &http.Client{
		Timeout: transactionTimeout,
	}

	// Initial check for any leftover events to send from last time
	eventCount, err := db.CountEventsWithAppServiceID(ctx, ws.AppService.ID)
	if err != nil {
		log.WithFields(log.Fields{
			"appservice": ws.AppService.ID,
		}).WithError(err).Fatal("appservice worker unable to read queued events from DB")
		return
	}
	ws.Cond.L.Lock()
	*ws.EventsReady = eventCount
	ws.Cond.L.Unlock()

	// Loop forever and keep waiting for more events to send
	for {
		// Wait for more events if we've sent all the events in the database
		ws.WaitForNewEvents()

		// Batch events up into a transaction
		eventsCount, txnID, maxEventID, transactionJSON, err := createTransaction(ctx, db, ws.AppService.ID)
		if err != nil {
			log.WithFields(log.Fields{
				"appservice": ws.AppService.ID,
			}).WithError(err).Fatal("appservice worker unable to create transaction")

			return
		}

		// Send the events off to the application service
		// Backoff if the application service does not respond
		err = send(client, ws.AppService, txnID, transactionJSON)
		if err != nil {
			// Backoff
			backoff(&ws, err)
			continue
		}

		// We sent successfully, hooray!
		ws.Backoff = 0

		ws.Cond.L.Lock()
		*ws.EventsReady -= eventsCount
		ws.Cond.L.Unlock()

		// Remove sent events from the DB
		err = db.RemoveEventsBeforeAndIncludingID(ctx, ws.AppService.ID, maxEventID)
		if err != nil {
			log.WithFields(log.Fields{
				"appservice": ws.AppService.ID,
			}).WithError(err).Fatal("unable to remove appservice events from the database")
			return
		}

		// Update transactionID
		currentTransactionID++
		if err = db.UpsertTxnIDWithAppServiceID(ctx, ws.AppService.ID, currentTransactionID); err != nil {
			log.WithFields(log.Fields{
				"appservice": ws.AppService.ID,
			}).WithError(err).Fatal("unable to update transaction ID")
			return
		}
	}
}

// backoff pauses the calling goroutine for a 2^some backoff exponent seconds
func backoff(ws *types.ApplicationServiceWorkerState, err error) {
	// Calculate how long to backoff for
	backoffDuration := time.Duration(math.Pow(2, float64(ws.Backoff)))
	backoffSeconds := time.Second * backoffDuration

	log.WithFields(log.Fields{
		"appservice": ws.AppService.ID,
	}).WithError(err).Warnf("unable to send transactions successfully, backing off for %ds",
		backoffDuration)

	ws.Backoff++
	if ws.Backoff > 6 {
		ws.Backoff = 6
	}

	// Backoff
	time.Sleep(backoffSeconds)
}

// createTransaction takes in a slice of AS events, stores them in an AS
// transaction, and JSON-encodes the results.
func createTransaction(
	ctx context.Context,
	db *storage.Database,
	appserviceID string,
) (
	eventsCount, txnID, maxID int,
	transactionJSON []byte,
	err error,
) {
	// Retrieve the latest events from the DB (will return old events if they weren't successfully sent)
	txnID, maxID, events, err := db.GetEventsWithAppServiceID(ctx, appserviceID, transactionBatchSize)
	if err != nil {
		log.WithFields(log.Fields{
			"appservice": appserviceID,
		}).WithError(err).Fatalf("appservice worker unable to read queued events from DB")

		return
	}

	// Check if these events already have a transaction ID
	if txnID == -1 {
		txnID = currentTransactionID
		// Mark new events with current transactionID
		err := db.UpdateTxnIDForEvents(ctx, appserviceID, maxID, currentTransactionID)
		if err != nil {
			return 0, 0, 0, nil, err
		}
	}

	// Create a transaction and store the events inside
	transaction := gomatrixserverlib.ApplicationServiceTransaction{
		Events: events,
	}

	transactionJSON, err = json.Marshal(transaction)
	if err != nil {
		return
	}

	eventsCount = len(events)
	return
}

// send sends events to an application service. Returns an error if an OK was not
// received back from the application service or the request timed out.
func send(
	client *http.Client,
	appservice config.ApplicationService,
	txnID int,
	transaction []byte,
) error {
	// POST a transaction to our AS
	address := fmt.Sprintf("%s/transactions/%d", appservice.URL, txnID)
	resp, err := client.Post(address, "application/json", bytes.NewBuffer(transaction))
	if err != nil {
		return err
	}
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			log.WithFields(log.Fields{
				"appservice": appservice.ID,
			}).WithError(err).Error("unable to close response body from application service")
		}
	}()

	// Check the AS received the events correctly
	if resp.StatusCode != http.StatusOK {
		// TODO: Handle non-200 error codes from application services
		return fmt.Errorf("non-OK status code %d returned from AS", resp.StatusCode)
	}

	return nil
}
