package icloudadapter

import (
	"errors"

	"github.com/torkve/icloud-notes/icloud"

	"recipes/internal/notesync"
)

// errBadSession is returned when a notesync.Session passed to the adapter is not
// one the adapter issued.
var errBadSession = errors.New("icloudadapter: session is not an adapter session")

// session adapts an authenticated icloud-notes Client to notesync.Session: it
// carries the live client for the provider methods and serializes the underlying
// account session for persistence.
type session struct {
	client *icloud.Client
}

// Bytes serializes the current account session for encrypted storage.
func (s *session) Bytes() ([]byte, error) { return s.client.Session().MarshalBinary() }

// Expired reports whether the account session's token window has elapsed.
func (s *session) Expired() bool { return s.client.Session().Expired() }

// clientOf extracts the live client from a session the adapter issued.
func clientOf(sess notesync.Session) (*icloud.Client, error) {
	s, ok := sess.(*session)
	if !ok {
		return nil, errBadSession
	}
	return s.client, nil
}
