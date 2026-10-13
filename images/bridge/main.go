// Command bridge simulates Founda's cloud component as an IHE XCA gateway.
//
// In gateway mode (the default) a bridge belongs to one hospital: it is the
// document registry its connector registers documents with (ITI-42), answers
// that connector's queries (ITI-18/ITI-43) and asks the central bridge for
// documents from other hospitals (ITI-38/ITI-39).
//
// In central mode the bridge keeps the directory of hospital bridges and routes
// cross-gateway queries and retrieves between them.
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

//go:embed index.html
var indexHTML string

// A hospital bridge counts as part of the network while it keeps registering.
const activeFor = 30 * time.Second

type community struct {
	Name     string
	Display  string
	URL      string
	LastSeen time.Time
}

type server struct {
	name       string // hospital name, or "central"
	display    string // hospital display name, e.g. Hospital A
	central    bool
	centralURL string // gateway mode: the central bridge
	selfURL    string // gateway mode: how the central bridge reaches this bridge
	tmpl       *template.Template
	log        xds.Log

	mu          sync.Mutex
	docs        map[string]xds.Document // gateway mode: the registry
	communities map[string]*community   // central mode: the directory
	registered  bool
	centralErr  string
}

func main() {
	mode := xds.Env("MODE", "gateway")
	if mode != "gateway" && mode != "central" {
		log.Fatalf("MODE must be gateway or central, got %q", mode)
	}
	port := xds.Env("PORT", "8080")
	name := xds.Env("HOSPITAL_NAME", "central")
	s := &server{
		name:        name,
		display:     xds.Env("HOSPITAL_DISPLAY", xds.DisplayName(name)),
		central:     mode == "central",
		centralURL:  strings.TrimRight(xds.Env("CENTRAL_URL", "http://bridge-central"), "/"),
		selfURL:     strings.TrimRight(xds.Env("SELF_URL", "http://localhost:"+port), "/"),
		tmpl:        template.Must(template.New("index").Funcs(template.FuncMap{"ago": ago}).Parse(indexHTML)),
		docs:        map[string]xds.Document{},
		communities: map[string]*community{},
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", web.Static())
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })

	if s.central {
		mux.HandleFunc("POST /directory/register", s.registerCommunity)
		mux.HandleFunc("POST /xca/query", s.routeQuery)
		mux.HandleFunc("POST /xca/retrieve", s.routeRetrieve)
	} else {
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
// so, so the central bridge knows which hospitals are online.
func (s *server) joinNetwork() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := xds.PostJSON(ctx, s.centralURL+"/directory/register", xds.RegisterCommunity{Community: s.name, Name: s.display, URL: s.selfURL}, nil)
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
		time.Sleep(10 * time.Second)
	}
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
	c, known := s.communities[reg.Community]
	rejoined := !known || time.Since(c.LastSeen) > activeFor
	s.communities[reg.Community] = &community{Name: reg.Community, Display: xds.Label(reg.Community, reg.Name), URL: reg.URL, LastSeen: time.Now()}
	s.mu.Unlock()
	if rejoined {
		s.log.Add(xds.Join, xds.Label(reg.Community, reg.Name)+" joined the network", true)
	}
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

type communityView struct {
	Name, Display, URL string
	Active             bool
	LastSeen           time.Time
}

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	patients := map[string]bool{}
	for _, d := range s.docs {
		patients[d.PatientID] = true
	}
	var communities []communityView
	for _, c := range s.communities {
		communities = append(communities, communityView{c.Name, c.Display, c.URL, time.Since(c.LastSeen) <= activeFor, c.LastSeen})
	}
	sort.Slice(communities, func(i, j int) bool { return communities[i].Name < communities[j].Name })
	data := map[string]any{
		"Display":      s.display,
		"Central":      s.central,
		"CentralURL":   s.centralURL,
		"Registered":   s.registered,
		"CentralErr":   s.centralErr,
		"Documents":    len(s.docs),
		"Patients":     len(patients),
		"Communities":  communities,
		"Events":       s.log.List(),
		"Transactions": xds.Transactions,
	}
	s.mu.Unlock()
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Print(err)
	}
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	if d < time.Second {
		return "just now"
	}
	return d.String() + " ago"
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
