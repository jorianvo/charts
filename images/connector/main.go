// Command connector simulates the Founda adapter inside a hospital. It receives
// HL7 v2 ORU^R01 messages from the hospital's EMR (simulated with a button),
// registers each document with its bridge (ITI-42) and lets a clinician look up
// a patient across the network through that bridge.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jorianvo/charts/images/internal/hl7"
	"github.com/jorianvo/charts/images/internal/web"
	"github.com/jorianvo/charts/images/internal/xds"
)

//go:embed index.html
var indexHTML string

// sendingApp names the simulated EMR in the HL7 messages it sends (MSH-3).
const sendingApp = "EMR"

type config struct {
	Hospital        string // e.g. hospital-a
	HospitalDisplay string // e.g. Hospital A, from HOSPITAL_DISPLAY
	BridgeEndpoint  string // the bridge's host, shown in the UI
	BridgeURL       string // the bridge's URL, e.g. http://bridge-hospital-a
}

// feedItem is one HL7 message received from the EMR.
type feedItem struct {
	Time        time.Time `json:"time"`
	ControlID   string    `json:"controlId"`
	Type        string    `json:"type"`
	PatientID   string    `json:"patientId"`
	PatientName string    `json:"patientName"`
	Title       string    `json:"title"`
	Raw         string    `json:"raw"`
	Status      string    `json:"status"`
	OK          bool      `json:"ok"`
}

type server struct {
	cfg  config
	tmpl *template.Template

	mu        sync.Mutex
	seq       int
	feed      []feedItem
	bridgeOK  bool
	bridgeErr string
}

func main() {
	hospital := xds.Env("HOSPITAL_NAME", "hospital-a")
	bridgeURL := strings.TrimRight(xds.Env("BRIDGE_URL", "http://localhost:8090"), "/")
	endpoint := bridgeURL
	if u, err := url.Parse(bridgeURL); err == nil && u.Host != "" {
		endpoint = u.Host
	}
	s := &server{
		cfg: config{
			Hospital:        hospital,
			HospitalDisplay: xds.Env("HOSPITAL_DISPLAY", xds.DisplayName(hospital)),
			BridgeEndpoint:  endpoint,
			BridgeURL:       bridgeURL,
		},
		tmpl: template.Must(template.New("index").Parse(indexHTML)),
		feed: []feedItem{},
	}
	go s.watchBridge()

	mux := http.NewServeMux()
	mux.Handle("GET /static/", web.Static())
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/emr/simulate", s.simulate)
	mux.HandleFunc("GET /api/patients/{id}/documents", s.query)
	mux.HandleFunc("GET /api/documents/{community}/{id}", s.retrieve)

	addr := ":" + xds.Env("PORT", "8080")
	log.Printf("connector for %s listening on %s, bridge %s", s.cfg.Hospital, addr, s.cfg.BridgeURL)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	if err := s.tmpl.Execute(w, s.cfg); err != nil {
		log.Print(err)
	}
}

func (s *server) state(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	xds.WriteJSON(w, http.StatusOK, map[string]any{
		"feed":     s.feed,
		"bridge":   map[string]any{"endpoint": s.cfg.BridgeEndpoint, "ok": s.bridgeOK, "error": s.bridgeErr},
		"patients": hl7.Patients,
	})
}

// simulate makes the EMR send a random report for a random patient.
func (s *server) simulate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seq++
	controlID := fmt.Sprintf("%s-%04d", strings.ToUpper(s.cfg.Hospital), s.seq)
	s.mu.Unlock()

	p := hl7.Patients[rand.IntN(len(hl7.Patients))]
	rep := hl7.Reports[rand.IntN(len(hl7.Reports))]
	raw := hl7.BuildORU(sendingApp, strings.ToUpper(s.cfg.Hospital), controlID, p, rep, time.Now())

	item, err := s.receive(r.Context(), raw)
	if err != nil {
		xds.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	xds.WriteJSON(w, http.StatusOK, item)
}

// receive parses an HL7 message, turns it into a document and registers that
// document with the bridge.
func (s *server) receive(ctx context.Context, raw string) (feedItem, error) {
	m, err := hl7.Parse(raw)
	if err != nil {
		return feedItem{}, err
	}
	item := feedItem{
		Time:        time.Now(),
		ControlID:   m.ControlID,
		Type:        m.Type,
		PatientID:   m.PatientID,
		PatientName: m.PatientName,
		Title:       m.DocTitle,
		Raw:         strings.ReplaceAll(strings.TrimSpace(raw), "\r", "\n"),
	}
	doc := xds.Document{
		ID:          fmt.Sprintf("%s.%s", s.cfg.Hospital, m.ControlID),
		PatientID:   m.PatientID,
		PatientName: m.PatientName,
		Title:       m.DocTitle,
		Code:        m.DocCode,
		Community:   s.cfg.Hospital,
		Source:      m.SendingApp,
		Created:     m.Time,
		Content:     m.Text,
	}
	if err := xds.PostJSON(ctx, s.cfg.BridgeURL+"/xds/register", doc, nil); err != nil {
		item.Status = "Bridge unreachable: " + err.Error()
	} else {
		item.OK = true
		item.Status = "Registered with the bridge"
	}

	s.mu.Lock()
	s.feed = append([]feedItem{item}, s.feed...)
	if len(s.feed) > 25 {
		s.feed = s.feed[:25]
	}
	s.mu.Unlock()
	return item, nil
}

// query asks the bridge for every document about a patient in the XDS network.
func (s *server) query(w http.ResponseWriter, r *http.Request) {
	var resp xds.QueryResponse
	u := s.cfg.BridgeURL + "/xds/query?patientId=" + url.QueryEscape(r.PathValue("id"))
	if err := xds.GetJSON(r.Context(), u, &resp); err != nil {
		xds.WriteError(w, http.StatusBadGateway, err)
		return
	}
	xds.WriteJSON(w, http.StatusOK, resp)
}

// retrieve fetches a single document, wherever in the network it lives.
func (s *server) retrieve(w http.ResponseWriter, r *http.Request) {
	var doc xds.Document
	u := fmt.Sprintf("%s/xds/retrieve?community=%s&id=%s", s.cfg.BridgeURL,
		url.QueryEscape(r.PathValue("community")), url.QueryEscape(r.PathValue("id")))
	if err := xds.GetJSON(r.Context(), u, &doc); err != nil {
		xds.WriteError(w, http.StatusBadGateway, err)
		return
	}
	xds.WriteJSON(w, http.StatusOK, doc)
}

// watchBridge keeps telling the bridge that this connector is running, so the
// central bridge can show it, and tracks whether the bridge is reachable.
func (s *server) watchBridge() {
	me := xds.Connector{Name: s.cfg.Hospital, Display: s.cfg.HospitalDisplay}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := xds.PostJSON(ctx, s.cfg.BridgeURL+"/connector/heartbeat", me, nil)
		cancel()
		s.mu.Lock()
		s.bridgeOK = err == nil
		s.bridgeErr = ""
		if err != nil {
			s.bridgeErr = err.Error()
		}
		s.mu.Unlock()
		time.Sleep(3 * time.Second)
	}
}
