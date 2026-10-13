// Package xds holds the small, simplified subset of IHE XDS/XCA that the demo
// connector and bridge speak to each other. Messages are JSON over HTTP; each
// step is labelled with the IHE transaction it stands for.
package xds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Transaction is a step in the document exchange, with the IHE transaction it
// stands for. The UIs show the plain name first, for an audience new to IHE.
type Transaction struct {
	Code    string `json:"code"`    // IHE transaction, e.g. ITI-42
	Name    string `json:"name"`    // plain-language name
	IHEName string `json:"iheName"` // name in the IHE profile
	Explain string `json:"explain"` // one-line explanation
}

var (
	ITI42 = Transaction{"ITI-42", "Register document", "Register Document Set",
		"The connector tells its hospital's bridge that a new document exists."}
	ITI18 = Transaction{"ITI-18", "Find documents", "Registry Stored Query",
		"The connector asks its bridge which documents exist for a patient."}
	ITI43 = Transaction{"ITI-43", "Open document", "Retrieve Document Set",
		"The connector fetches a document its own hospital holds."}
	ITI38 = Transaction{"ITI-38", "Ask other hospitals", "Cross Gateway Query",
		"A bridge asks the bridges of other hospitals which documents they have for a patient."}
	ITI39 = Transaction{"ITI-39", "Fetch from other hospital", "Cross Gateway Retrieve",
		"A bridge fetches a document that another hospital holds."}
	Join = Transaction{"", "Join network", "",
		"A hospital's bridge registers with the central bridge, so other hospitals can find it."}
	Connect = Transaction{"", "Connect hospital", "",
		"The connector on the hospital's Talos cluster reaches its bridge in the cloud, over the hospital's site-to-site VPN."}

	// Transactions lists every step, in the order they happen in the demo.
	Transactions = []Transaction{ITI42, ITI18, ITI38, ITI39, ITI43, Join, Connect}
)

// Document is a clinical document. Query results leave Content empty.
type Document struct {
	ID            string    `json:"id"`
	PatientID     string    `json:"patientId"`
	PatientName   string    `json:"patientName"`
	Title         string    `json:"title"`
	Code          string    `json:"code"`
	Community     string    `json:"community"`
	CommunityName string    `json:"communityName,omitempty"` // display name of the community
	Source        string    `json:"source"`
	Created       time.Time `json:"created"`
	Content       string    `json:"content,omitempty"`
}

// Entry returns the document metadata without its content.
func (d Document) Entry() Document {
	d.Content = ""
	return d
}

type QueryRequest struct {
	PatientID string `json:"patientId"`
	From      string `json:"from"`
	FromName  string `json:"fromName,omitempty"`
}

type QueryResponse struct {
	Documents []Document `json:"documents"`
	Errors    []string   `json:"errors,omitempty"`
}

type RetrieveRequest struct {
	Community  string `json:"community"`
	DocumentID string `json:"documentId"`
	From       string `json:"from"`
	FromName   string `json:"fromName,omitempty"`
}

type RegisterCommunity struct {
	Community string     `json:"community"`
	Name      string     `json:"name,omitempty"`
	URL       string     `json:"url"`
	Connector *Connector `json:"connector,omitempty"` // set while the hospital's connector reaches the bridge
}

// Connector describes a hospital's connector. The connector sends it to its
// bridge as a heartbeat, and the bridge passes it on to the central bridge.
type Connector struct {
	Name    string `json:"name"`              // e.g. hospital-a-adapter
	Display string `json:"display,omitempty"` // e.g. Hospital A Adapter
}

// Event is one line in a component's transaction log.
type Event struct {
	Time        time.Time   `json:"time"`
	Transaction Transaction `json:"transaction"`
	Detail      string      `json:"detail"`
	OK          bool        `json:"ok"`
}

// Log keeps the most recent events, newest first.
type Log struct {
	mu     sync.Mutex
	events []Event
}

const maxEvents = 40

func (l *Log) Add(transaction Transaction, detail string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append([]Event{{Time: time.Now(), Transaction: transaction, Detail: detail, OK: ok}}, l.events...)
	if len(l.events) > maxEvents {
		l.events = l.events[:maxEvents]
	}
}

func (l *Log) List() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...)
}

// Client sends every request to other components.
var Client = &http.Client{Timeout: 5 * time.Second}

// PostJSON sends in as JSON and decodes the response into out (if not nil).
func PostJSON(ctx context.Context, url string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(req, out)
}

// GetJSON decodes the JSON response of a GET request into out.
func GetJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return do(req, out)
}

func do(req *http.Request, out any) error {
	resp, err := Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, err error) {
	WriteJSON(w, status, map[string]string{"error": err.Error()})
}

// Env returns the environment variable key, or def when it is unset or empty.
func Env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Label returns display, or the display name of name when display is empty.
func Label(name, display string) string {
	if display != "" {
		return display
	}
	return DisplayName(name)
}

// DisplayName turns "hospital-a" into "Hospital A".
func DisplayName(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
