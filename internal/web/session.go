package web

import (
	"net/http"

	"github.com/gorilla/sessions"
)

const (
	sessionName    = "recipes_session"
	sessionUserKey = "uid"
)

// login records the authenticated user id in the session cookie. Clearing the
// session ID forces the FilesystemStore to mint a fresh one on Save, rotating
// the session on privilege change to prevent session fixation.
func (s *Server) login(w http.ResponseWriter, r *http.Request, userID int64) error {
	sess, _ := s.sessions.Get(r, sessionName)
	sess.ID = ""
	sess.Values[sessionUserKey] = userID
	return sess.Save(r, w)
}

// logout clears the session.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	sess, _ := s.sessions.Get(r, sessionName)
	delete(sess.Values, sessionUserKey)
	sess.Options.MaxAge = -1 // expire the cookie
	return sess.Save(r, w)
}

// sessionUserID returns the logged-in user id and whether one is present.
func (s *Server) sessionUserID(r *http.Request) (int64, bool) {
	sess, err := s.sessions.Get(r, sessionName)
	if err != nil {
		return 0, false
	}
	v, ok := sess.Values[sessionUserKey]
	if !ok {
		return 0, false
	}
	id, ok := v.(int64)
	return id, ok
}

// newSessionStore builds a filesystem-backed cookie session store.
func newSessionStore(dir string, authKey, encKey []byte, secure bool) *sessions.FilesystemStore {
	st := sessions.NewFilesystemStore(dir, authKey, encKey)
	st.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 30, // 30 days
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
	// Keep encoded session bytes well under the 4KB cookie limit (we only store a uid).
	st.MaxLength(4096)
	return st
}
