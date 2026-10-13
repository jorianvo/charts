// Command bridge simulates Founda's cloud component as an IHE XCA gateway.
//
// In gateway mode (the default) a bridge belongs to one hospital: it is the
// document registry its connector registers documents with (ITI-42), answers
// that connector's queries (ITI-18/ITI-43) and asks the central bridge for
// documents from other hospitals (ITI-38/ITI-39).
//
// In central mode the bridge keeps the directory of hospital bridges and routes
// cross-gateway queries and retrieves between them. Its page draws the network:
// the cloud, every hospital's on-prem Talos cluster and the VPNs between them.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jorianvo/charts/images/internal/web"
	"github.com/jorianvo/charts/images/internal/xds"
)

var (
	//go:embed index.html
	indexHTML string
	//go:embed central.html
	centralHTML string
)

// A hospital bridge counts as part of the network while it keeps registering,
// and a connector as connected while it keeps sending heartbeats.
const activeFor = 30 * time.Second

type community struct {
	Name      string
	Display   string
	URL       string
	LastSeen  time.Time
	Connector *xds.Connector // nil while the hospital's connector does not reach the bridge
}

type server struct {
	name       string // hospital name, or "central"
	display    string // hospital display name, e.g. Hospital A
	central    bool
	centralURL string   // gateway mode: the central bridge
	selfURL    string   // gateway mode: how the central bridge reaches this bridge
	hospitals  []string // central mode: the hospitals to draw, connected or not
	onboard    string   // central mode: the hospital the page walks through onboarding
	tmpl       *template.Template
	log        xds.Log

	mu            sync.Mutex
	docs          map[string]xds.Document // gateway mode: the registry
	connector     *xds.Connector          // gateway mode: this hospital's connector
	connectorSeen time.Time
	communities   map[string]*community // central mode: the directory
	step          int                   // central mode: onboarding steps the presenter has clicked, 0 to 2
	registered    bool
	centralErr    string
}

func main() {
	mode := xds.Env("MODE", "gateway")
	if mode != "gateway" && mode != "central" {
		log.Fatalf("MODE must be gateway or central, got %q", mode)
	}
	port := xds.Env("PORT", "8080")
	name := xds.Env("HOSPITAL_NAME", "central")
	page := indexHTML
	if mode == "central" {
		page = centralHTML
	}
	s := &server{
		name:        name,
		display:     xds.Env("HOSPITAL_DISPLAY", xds.DisplayName(name)),
		central:     mode == "central",
		centralURL:  strings.TrimRight(xds.Env("CENTRAL_URL", "http://bridge-central"), "/"),
		selfURL:     strings.TrimRight(xds.Env("SELF_URL", "http://localhost:"+port), "/"),
		hospitals:   strings.FieldsFunc(xds.Env("HOSPITALS", ""), func(r rune) bool { return r == ',' || r == ' ' }),
		onboard:     xds.Env("ONBOARD", ""),
		tmpl:        template.Must(template.New("index").Parse(page)),
		docs:        map[string]xds.Document{},
		communities: map[string]*community{},
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", web.Static())
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })

	if s.central {
		mux.HandleFunc("GET /api/topology", s.topology)
		mux.HandleFunc("POST /api/step", s.setStep)
		mux.HandleFunc("POST /directory/register", s.registerCommunity)
		mux.HandleFunc("POST /xca/query", s.routeQuery)
		mux.HandleFunc("POST /xca/retrieve", s.routeRetrieve)
	} else {
		mux.HandleFunc("POST /connector/heartbeat", s.connectorHeartbeat)
		mux.HandleFunc("POST /xds/register", s.registerDocument)
		mux.HandleFunc("GET /xds/query", s.query)
		mux.HandleFunc("GET /xds/retrieve", s.retrieve)
		mux.HandleFunc("POST /xca/query", s.respondQuery)
		mux.HandleFunc("POST /xca/retrieve", s.respondRetrieve)
		go s.joinNetwork()
	}

	log.Printf("bridge %s (%s) listening on :%s", s.name, mode, port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// --- Gateway mode ---------------------------------------------------------

// joinNetwork registers this bridge with the central bridge, and keeps doing
// so, so the central bridge knows which hospitals and connectors are online.
func (s *server) joinNetwork() {
	connected := false
	for {
		s.mu.Lock()
		conn := s.connectedConnector()
		s.mu.Unlock()
		if connected && conn == nil {
			s.log.Add(xds.Connect, "Lost the connector in the hospital", false)
		}
		connected = conn != nil

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := xds.PostJSON(ctx, s.centralURL+"/directory/register", xds.RegisterCommunity{Community: s.name, Name: s.display, URL: s.selfURL, Connector: conn}, nil)
		cancel()

		s.mu.Lock()
		was := s.registered
		s.registered = err == nil
		s.centralErr = ""
		if err != nil {
			s.centralErr = err.Error()
		}
		s.mu.Unlock()

		switch {
		case err == nil && !was:
			s.log.Add(xds.Join, "Registered with the central bridge", true)
		case err != nil && was:
			s.log.Add(xds.Join, "Lost the central bridge: "+err.Error(), false)
		}
		time.Sleep(3 * time.Second)
	}
}

// connectorHeartbeat records that the hospital's connector reaches this bridge.
func (s *server) connectorHeartbeat(w http.ResponseWriter, r *http.Request) {
	var c xds.Connector
	if err := decode(r, &c); err != nil || c.Name == "" {
		xds.WriteError(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	s.mu.Lock()
	was := s.connectedConnector() != nil
	s.connector, s.connectorSeen = &c, time.Now()
	s.mu.Unlock()
	if !was {
		s.log.Add(xds.Connect, xds.Label(c.Name, c.Display)+" connected from the hospital", true)
	}
	w.WriteHeader(http.StatusNoContent)
}

// connectedConnector returns the hospital's connector while it keeps sending
// heartbeats, or nil. The caller holds s.mu.
func (s *server) connectedConnector() *xds.Connector {
	if s.connector == nil || time.Since(s.connectorSeen) > activeFor {
		return nil
	}
	c := *s.connector
	return &c
}

// registerDocument stores a document the hospital's connector sends (ITI-42).
func (s *server) registerDocument(w http.ResponseWriter, r *http.Request) {
	var doc xds.Document
	if err := decode(r, &doc); err != nil {
		xds.WriteError(w, http.StatusBadRequest, err)
		return
	}
	doc.Community = s.name
	doc.CommunityName = s.display
	s.mu.Lock()
	s.docs[doc.ID] = doc
	s.mu.Unlock()
	s.log.Add(xds.ITI42, fmt.Sprintf("%s for %s, from the connector", doc.Title, doc.PatientName), true)
	w.WriteHeader(http.StatusNoContent)
}

// query answers the connector with this hospital's documents for a patient,
// plus those of every other hospital, found through the central bridge.
func (s *server) query(w http.ResponseWriter, r *http.Request) {
	pid := r.URL.Query().Get("patientId")
	local := s.local(pid)
	s.log.Add(xds.ITI18, fmt.Sprintf("Connector asks for patient %s: %d local document(s)", pid, len(local)), true)

	resp := xds.QueryResponse{Documents: local}
	var remote xds.QueryResponse
	err := xds.PostJSON(r.Context(), s.centralURL+"/xca/query", xds.QueryRequest{PatientID: pid, From: s.name, FromName: s.display}, &remote)
	if err != nil {
		s.log.Add(xds.ITI38, "Central bridge unreachable: "+err.Error(), false)
		resp.Errors = append(resp.Errors, "Other hospitals unreachable: "+err.Error())
	} else {
		s.log.Add(xds.ITI38, fmt.Sprintf("Central bridge returned %d document(s) from other hospitals", len(remote.Documents)), true)
		resp.Documents = append(resp.Documents, remote.Documents...)
		resp.Errors = append(resp.Errors, remote.Errors...)
	}
	xds.WriteJSON(w, http.StatusOK, resp)
}

// retrieve returns a document to the connector, fetching it through the
// central bridge when another hospital holds it.
func (s *server) retrieve(w http.ResponseWriter, r *http.Request) {
	from, id := r.URL.Query().Get("community"), r.URL.Query().Get("id")
	if from == s.name {
		doc, ok := s.get(id)
		if !ok {
			xds.WriteError(w, http.StatusNotFound, errors.New("document not found"))
			return
		}
		s.log.Add(xds.ITI43, fmt.Sprintf("Connector opens %s for %s", doc.Title, doc.PatientName), true)
		xds.WriteJSON(w, http.StatusOK, doc)
		return
	}
	var doc xds.Document
	err := xds.PostJSON(r.Context(), s.centralURL+"/xca/retrieve", xds.RetrieveRequest{Community: from, DocumentID: id, From: s.name, FromName: s.display}, &doc)
	if err != nil {
		s.log.Add(xds.ITI39, fmt.Sprintf("Retrieve from %s failed: %v", xds.DisplayName(from), err), false)
		xds.WriteError(w, http.StatusBadGateway, err)
		return
	}
	s.log.Add(xds.ITI39, fmt.Sprintf("Retrieved %s for %s from %s", doc.Title, doc.PatientName, xds.Label(doc.Community, doc.CommunityName)), true)
	xds.WriteJSON(w, http.StatusOK, doc)
}

// respondQuery answers a cross-gateway query from another hospital (ITI-38).
func (s *server) respondQuery(w http.ResponseWriter, r *http.Request) {
	var q xds.QueryRequest
	if err := decode(r, &q); err != nil {
		xds.WriteError(w, http.StatusBadRequest, err)
		return
	}
	docs := s.local(q.PatientID)
	s.log.Add(xds.ITI38, fmt.Sprintf("%s asks for patient %s: answered with %d document(s)", xds.Label(q.From, q.FromName), q.PatientID, len(docs)), true)
	xds.WriteJSON(w, http.StatusOK, xds.QueryResponse{Documents: docs})
}

// respondRetrieve sends a document to another hospital (ITI-39).
func (s *server) respondRetrieve(w http.ResponseWriter, r *http.Request) {
	var q xds.RetrieveRequest
	if err := decode(r, &q); err != nil {
		xds.WriteError(w, http.StatusBadRequest, err)
		return
	}
	doc, ok := s.get(q.DocumentID)
	if !ok {
		s.log.Add(xds.ITI39, fmt.Sprintf("%s asks for unknown document %s", xds.Label(q.From, q.FromName), q.DocumentID), false)
		xds.WriteError(w, http.StatusNotFound, errors.New("document not found"))
		return
	}
	s.log.Add(xds.ITI39, fmt.Sprintf("Sent %s for %s to %s", doc.Title, doc.PatientName, xds.Label(q.From, q.FromName)), true)
	xds.WriteJSON(w, http.StatusOK, doc)
}

func (s *server) local(pid string) []xds.Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	docs := []xds.Document{}
	for _, d := range s.docs {
		if d.PatientID == pid {
			docs = append(docs, d.Entry())
		}
	}
	return docs
}

func (s *server) get(id string) (xds.Document, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[id]
	return d, ok
}

// --- Central mode ---------------------------------------------------------

func (s *server) registerCommunity(w http.ResponseWriter, r *http.Request) {
	var reg xds.RegisterCommunity
	if err := decode(r, &reg); err != nil || reg.Community == "" || reg.URL == "" {
		xds.WriteError(w, http.StatusBadRequest, errors.New("community and url are required"))
		return
	}
	s.mu.Lock()
	s.communities[reg.Community] = &community{Name: reg.Community, Display: xds.Label(reg.Community, reg.Name), URL: reg.URL, LastSeen: time.Now(), Connector: reg.Connector}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// routeQuery forwards a cross-gateway query to every other active hospital and
// combines their answers.
func (s *server) routeQuery(w http.ResponseWriter, r *http.Request) {
	var q xds.QueryRequest
	if err := decode(r, &q); err != nil {
		xds.WriteError(w, http.StatusBadRequest, err)
		return
	}
	peers := slices.DeleteFunc(s.active(), func(c community) bool { return c.Name == q.From })
	from := xds.Label(q.From, q.FromName)

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		resp = xds.QueryResponse{Documents: []xds.Document{}}
	)
	for _, peer := range peers {
		wg.Go(func() {
			var res xds.QueryResponse
			err := xds.PostJSON(r.Context(), peer.URL+"/xca/query", q, &res)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				s.log.Add(xds.ITI38, fmt.Sprintf("%s → %s failed: %v", from, peer.Display, err), false)
				resp.Errors = append(resp.Errors, fmt.Sprintf("%s: %v", peer.Display, err))
				return
			}
			s.log.Add(xds.ITI38, fmt.Sprintf("%s → %s: %d document(s) for patient %s", from, peer.Display, len(res.Documents), q.PatientID), true)
			resp.Documents = append(resp.Documents, res.Documents...)
		})
	}
	wg.Wait()
	if len(peers) == 0 {
		s.log.Add(xds.ITI38, from+" asked, but no other hospital is connected", true)
	}
	xds.WriteJSON(w, http.StatusOK, resp)
}

// routeRetrieve forwards a cross-gateway retrieve to the hospital holding the document.
func (s *server) routeRetrieve(w http.ResponseWriter, r *http.Request) {
	var q xds.RetrieveRequest
	if err := decode(r, &q); err != nil {
		xds.WriteError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	c, ok := s.communities[q.Community]
	s.mu.Unlock()
	if !ok {
		xds.WriteError(w, http.StatusNotFound, fmt.Errorf("unknown community %q", q.Community))
		return
	}
	from := xds.Label(q.From, q.FromName)
	var doc xds.Document
	if err := xds.PostJSON(r.Context(), c.URL+"/xca/retrieve", q, &doc); err != nil {
		s.log.Add(xds.ITI39, fmt.Sprintf("%s ← %s failed: %v", from, c.Display, err), false)
		xds.WriteError(w, http.StatusBadGateway, err)
		return
	}
	s.log.Add(xds.ITI39, fmt.Sprintf("%s ← %s: %s", from, c.Display, doc.Title), true)
	xds.WriteJSON(w, http.StatusOK, doc)
}

func (s *server) active() []community {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []community
	for _, c := range s.communities {
		if time.Since(c.LastSeen) <= activeFor {
			out = append(out, *c)
		}
	}
	return out
}

// --- UI -------------------------------------------------------------------

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	patients := map[string]bool{}
	for _, d := range s.docs {
		patients[d.PatientID] = true
	}
	data := map[string]any{
		"Display":      s.display,
		"Onboard":      s.onboard,
		"Registered":   s.registered,
		"CentralErr":   s.centralErr,
		"Connector":    s.connectedConnector(),
		"Documents":    len(s.docs),
		"Patients":     len(patients),
		"Events":       s.log.List(),
		"Transactions": xds.Transactions,
	}
	s.mu.Unlock()
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Print(err)
	}
}

// site is a hospital as the central page draws it: its on-prem Talos cluster
// with the connector, and its bridge in the cloud. Bridge and Connector are nil
// while they are not deployed or not reachable.
//
// Cluster, VPN and Argo are the onboarding steps before anything is deployed:
// a Talos cluster runs in the hospital, its site-to-site VPN is up and the
// cluster is registered with Argo CD. They are always done for a hospital that
// is not being onboarded. For the one that is, the presenter clicks through the
// first two, and the cluster counts as registered once Argo CD deployed the
// connector on it. A connector reaching its bridge proves all three.
type site struct {
	Name      string         `json:"name"`
	Display   string         `json:"display"`
	Cluster   bool           `json:"cluster"`
	VPN       bool           `json:"vpn"`
	Argo      bool           `json:"argo"`
	Bridge    *bridgeView    `json:"bridge"`
	Connector *xds.Connector `json:"connector"`
}

type bridgeView struct {
	Display string `json:"display"`
}

// sites returns the configured hospitals, in order, followed by any other
// hospital whose bridge has registered.
func (s *server) sites() []site {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := slices.Clone(s.hospitals)
	var others []string
	for name := range s.communities {
		if !slices.Contains(names, name) {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	names = append(names, others...)

	out := make([]site, 0, len(names))
	for _, name := range names {
		h := site{Name: name, Display: xds.DisplayName(name)}
		if c, ok := s.communities[name]; ok && time.Since(c.LastSeen) <= activeFor {
			h.Bridge = &bridgeView{Display: c.Display}
			h.Connector = c.Connector
		}
		connected := h.Connector != nil
		if name == s.onboard {
			h.Cluster = s.step >= 1 || connected
			h.VPN = s.step >= 2 || connected
			h.Argo = connected
		} else {
			h.Cluster, h.VPN, h.Argo = true, connected, true
		}
		out = append(out, h)
	}
	return out
}

// topology feeds the central page: every hospital and what runs where.
func (s *server) topology(w http.ResponseWriter, _ *http.Request) {
	sites := s.sites()
	s.mu.Lock()
	step := s.step
	s.mu.Unlock()
	xds.WriteJSON(w, http.StatusOK, map[string]any{
		"hospitals": sites,
		"onboard":   s.onboard,
		"step":      step,
	})
}

// setStep records how far the presenter clicked through onboarding: 0 to 2.
// The last two steps, joining Argo CD and pushing a folder, are done once Argo
// CD deploys the hospital's connector.
func (s *server) setStep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Step int `json:"step"`
	}
	if err := decode(r, &req); err != nil || req.Step < 0 || req.Step > 2 {
		xds.WriteError(w, http.StatusBadRequest, errors.New("step must be 0 to 2"))
		return
	}
	s.mu.Lock()
	s.step = req.Step
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
