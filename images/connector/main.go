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
	"net"
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

type config struct {
	Hospital        string // e.g. hospital-a
	HospitalDisplay string // e.g. Hospital A, from HOSPITAL_DISPLAY
	EMR             string // name of the hospital's EMR
	BridgeEndpoint  string // the bridge's Tailscale name, shown in the UI
	BridgeURL       string // the bridge's URL, e.g. https://hospital-a-bridge.example.ts.net
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
	endpoint := xds.Env("BRIDGE_ENDPOINT", "localhost:8090")
	s := &server{
		cfg: config{
			Hospital:        hospital,
			HospitalDisplay: xds.Env("HOSPITAL_DISPLAY", xds.DisplayName(hospital)),
			EMR:             xds.Env("EMR_NAME", "EMR"),
			BridgeEndpoint:  endpoint,
			BridgeURL:       strings.TrimRight(xds.Env("BRIDGE_URL", "http://"+endpoint), "/"),
		},
		tmpl: template.Must(template.New("index").Parse(indexHTML)),
		feed: []feedItem{},
	}
	// In Kubernetes the bridge is reached through a Tailscale egress Service.
	// Dial that Service, but keep the bridge's name for TLS and the Host header.
	if dial := xds.Env("BRIDGE_DIAL", ""); dial != "" {
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		xds.Client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, dial)
			},
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 5 * time.Second,
		}
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
	log.Printf("connector for %s (EMR %s) listening on %s, bridge %s", s.cfg.Hospital, s.cfg.EMR, addr, s.cfg.BridgeURL)
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
	raw := hl7.BuildORU(s.cfg.EMR, strings.ToUpper(s.cfg.Hospital), controlID, p, rep, time.Now())

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

// watchBridge keeps track of whether the bridge is reachable.
func (s *server) watchBridge() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := xds.GetJSON(ctx, s.cfg.BridgeURL+"/healthz", nil)
		cancel()
		s.mu.Lock()
		s.bridgeOK = err == nil
		s.bridgeErr = ""
		if err != nil {
			s.bridgeErr = err.Error()
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Second)
	}
}
