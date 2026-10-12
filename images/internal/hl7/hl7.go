// Package hl7 builds and parses the HL7 v2.5 ORU^R01 messages that the
// simulated EMR sends to the connector. It only handles the fields the demo uses.
package hl7

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const timeLayout = "20060102150405"

// Message is the part of an ORU^R01 message the connector cares about.
type Message struct {
	Type            string // MSH-9, e.g. ORU^R01
	ControlID       string // MSH-10
	SendingApp      string // MSH-3, the EMR
	SendingFacility string // MSH-4, the hospital
	Time            time.Time
	PatientID       string // PID-3
	PatientName     string // PID-5, as "Given Family"
	DocCode         string // OBR-4.1
	DocTitle        string // OBR-4.2
	Text            string // OBX-5, joined
}

// Parse reads an HL7 v2 message with segments separated by CR, LF or CRLF.
func Parse(raw string) (Message, error) {
	var m Message
	raw = strings.NewReplacer("\r\n", "\r", "\n", "\r").Replace(strings.TrimSpace(raw))
	var text []string
	for _, seg := range strings.Split(raw, "\r") {
		f := strings.Split(seg, "|")
		switch f[0] {
		case "MSH":
			// MSH-1 is the field separator itself, so MSH-n is f[n-1].
			m.SendingApp = field(f, 2)
			m.SendingFacility = field(f, 3)
			m.Time, _ = time.ParseInLocation(timeLayout, field(f, 6), time.Local)
			m.Type = field(f, 8)
			m.ControlID = field(f, 9)
		case "PID":
			m.PatientID = component(field(f, 3), 0)
			name := field(f, 5)
			m.PatientName = strings.TrimSpace(component(name, 1) + " " + component(name, 0))
		case "OBR":
			m.DocCode = component(field(f, 4), 0)
			m.DocTitle = component(field(f, 4), 1)
		case "OBX":
			text = append(text, strings.ReplaceAll(field(f, 5), `\.br\`, "\n"))
		}
	}
	m.Text = strings.Join(text, "\n")
	switch {
	case m.ControlID == "":
		return m, errors.New("missing MSH segment or control ID")
	case m.PatientID == "":
		return m, errors.New("missing PID-3 patient identifier")
	case m.DocTitle == "" && m.DocCode == "":
		return m, errors.New("missing OBR-4 document type")
	}
	if m.Time.IsZero() {
		m.Time = time.Now()
	}
	return m, nil
}

// Patient is a (synthetic) patient known to every simulated EMR.
type Patient struct {
	ID, Family, Given, BirthDate, Sex string
}

// Report is a (synthetic) document an EMR can send.
type Report struct {
	Code, Title, Text string
}

// BuildORU creates an ORU^R01 message from an EMR.
func BuildORU(emr, facility, controlID string, p Patient, r Report, t time.Time) string {
	ts := t.Format(timeLayout)
	segs := []string{
		fmt.Sprintf(`MSH|^~\&|%s|%s|FOUNDA|%s|%s||ORU^R01|%s|P|2.5`, emr, facility, facility, ts, controlID),
		fmt.Sprintf("PID|1||%s^^^%s^MR||%s^%s||%s|%s", p.ID, facility, p.Family, p.Given, p.BirthDate, p.Sex),
		fmt.Sprintf("OBR|1||%s|%s^%s^L|||%s", controlID, r.Code, r.Title, ts),
	}
	for i, line := range strings.Split(r.Text, "\n") {
		segs = append(segs, fmt.Sprintf("OBX|%d|TX|REPORT^Report||%s||||||F", i+1, line))
	}
	return strings.Join(segs, "\r") + "\r"
}

func field(f []string, i int) string {
	if i < len(f) {
		return f[i]
	}
	return ""
}

func component(s string, i int) string {
	return field(strings.Split(s, "^"), i)
}

// Synthetic demo data. Every hospital's EMR knows the same patients, so a
// clinician in one hospital can find documents another hospital registered.
var (
	Patients = []Patient{
		{"900001", "Garcia", "Maria", "19780412", "F"},
		{"900002", "Tanaka", "Kenji", "19650921", "M"},
		{"900003", "Okafor", "Amara", "19911103", "F"},
		{"900004", "Schmidt", "Lukas", "19530218", "M"},
	}
	Reports = []Report{
		{"CT-THX", "CT thorax", "Indication: persistent cough.\nFindings: no pulmonary nodules or consolidation, no pleural effusion.\nConclusion: no abnormalities."},
		{"XR-KNEE", "X-ray left knee", "Indication: pain after fall.\nFindings: no fracture, mild degenerative changes of the medial compartment.\nConclusion: early osteoarthritis."},
		{"MR-BRAIN", "MRI brain", "Indication: recurring headaches.\nFindings: no space-occupying lesion, normal ventricles.\nConclusion: no intracranial abnormality."},
		{"LAB-HBA1C", "Lab result HbA1c", "HbA1c: 44 mmol/mol (6.2%), reference below 42 mmol/mol (6.0%).\nComment: elevated, consistent with prediabetes."},
		{"DISCHARGE", "Discharge letter", "Admitted for observation after syncope.\nECG and telemetry unremarkable.\nDischarged home, follow-up with primary care physician in two weeks."},
	}
)
