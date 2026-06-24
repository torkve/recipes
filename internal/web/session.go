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

const (
	bindHandleKey  = "bind_handle"
	bindAppleIDKey = "bind_appleid"
)

// setBindHandle stashes the pending 2FA continuation in the user's session.
// The handle transitively carries auth material, so it must never go in a form.
func (s *Server) setBindHandle(w http.ResponseWriter, r *http.Request, appleID, handle string) error {
	sess, _ := s.sessions.Get(r, sessionName)
	sess.Values[bindHandleKey] = handle
	sess.Values[bindAppleIDKey] = appleID
	return sess.Save(r, w)
}

// getBindHandle returns the pending bind handle and apple id, if present.
func (s *Server) getBindHandle(r *http.Request) (appleID, handle string, ok bool) {
	sess, err := s.sessions.Get(r, sessionName)
	if err != nil {
		return "", "", false
	}
	h, ok1 := sess.Values[bindHandleKey].(string)
	a, _ := sess.Values[bindAppleIDKey].(string)
	if !ok1 || h == "" {
		return "", "", false
	}
	return a, h, true
}

// clearBindHandle removes any pending bind continuation.
func (s *Server) clearBindHandle(w http.ResponseWriter, r *http.Request) {
	sess, _ := s.sessions.Get(r, sessionName)
	delete(sess.Values, bindHandleKey)
	delete(sess.Values, bindAppleIDKey)
	_ = sess.Save(r, w)
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
	// FilesystemStore keeps session values in a file on disk (only the session id
	// is in the browser cookie), so this limit guards the on-disk record, not the
	// cookie. Allow enough room for the iCloud 2FA continuation (the captured
	// idmsa session: scnt, tokens and cookies — several KB).
	st.MaxLength(1 << 20)
	return st
}
