package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Fingerprint is the part of a Request that identifies what the model was
// asked: the model name, the system prompt, the whole conversation, and the
// names of the tools offered.
//
// It exists as a type rather than as bytes inside a hash function because a
// cassette records it (internal/model/cassette). A replay miss is then a diff
// between two of these rather than two hashes that merely differ, which is the
// difference between "the cassette is stale" and "the cassette is stale at
// message 6, where the tool result changed".
//
// Tool schemas and descriptions are deliberately not part of it — only names.
// That is what M1 chose for the event-log fingerprint and M5 keeps, with one
// consequence worth knowing: editing a tool's description changes what the
// model was asked and does not change this hash, so a cassette recorded before
// the edit still matches. Adding, removing, or renaming a tool does change it.
type Fingerprint struct {
	Model    string    `json:"model"`
	System   string    `json:"system"`
	Messages []Message `json:"messages"`
	Tools    []string  `json:"tools"`
}

// NewFingerprint projects a Request onto the fields that identify it.
func NewFingerprint(req Request) Fingerprint {
	names := make([]string, len(req.Tools))
	for i, t := range req.Tools {
		names[i] = t.Name
	}
	return Fingerprint{
		Model:    req.Model,
		System:   req.System,
		Messages: req.Messages,
		Tools:    names,
	}
}

// Hash is the hex SHA-256 of the fingerprint's canonical JSON.
func (f Fingerprint) Hash() string {
	raw, _ := json.Marshal(f)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// HashRequest fingerprints what the model was asked. It is written into every
// model_requested event as messages_sha256, and it is what a cassette matches
// on after normalisation, so the event log and the cassette cannot drift apart
// in how they identify a call.
func HashRequest(req Request) string { return NewFingerprint(req).Hash() }
