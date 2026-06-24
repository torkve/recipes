package web

import (
	"errors"
	"net/http"
	"strings"

	"recipes/internal/notesync"
	"recipes/internal/store"
)

var syncMessages = map[string]string{
	"bound":     "Аккаунт привязан",
	"folderset": "Папка для синхронизации сохранена",
	"pulled":    "Синхронизация из iCloud выполнена",
	"pushed":    "Изменения отправлены в iCloud",
	"resolved":  "Конфликт разрешён",
	"unbound":   "Аккаунт отвязан",
	"creds":     "Укажите Apple ID и пароль",
	"binderr":   "Не удалось войти в iCloud",
	"2faerr":    "Неверный код подтверждения",
	"nohandle":  "Сессия входа истекла, начните заново",
	"nofolder":  "Укажите папку",
	"pullerr":   "Ошибка синхронизации из iCloud",
	"pusherr":   "Ошибка отправки в iCloud",
	"needsbind": "Сначала привяжите аккаунт iCloud",
}

func (s *Server) syncEnabled() bool { return s.engine != nil }

func (s *Server) redirectSync(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/admin/sync?msg="+msg, http.StatusSeeOther)
}

// handleSyncStatus shows the iCloud binding state, folder choice, sync actions
// and unresolved conflicts.
func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	if !s.syncEnabled() {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID

	data := s.newPageData(r)
	data["Title"] = "Синхронизация с iCloud"
	if m, ok := syncMessages[r.URL.Query().Get("msg")]; ok {
		data["Message"] = m
	}

	acct, err := s.store.GetICloudAccount(ctx, uid)
	bound := err == nil
	data["Bound"] = bound
	if bound {
		data["AppleID"] = acct.AppleID
		data["Folder"] = acct.NotesFolder
		data["HasSession"] = len(acct.SessionBlob) > 0

		if len(acct.SessionBlob) > 0 {
			if folders, ferr := s.engine.ListRemoteFolders(ctx, uid); ferr == nil {
				data["Folders"] = folders
			} else {
				data["FolderError"] = ferr.Error()
			}
		}
	}

	if _, _, ok := s.getBindHandle(r); ok {
		data["Pending2FA"] = true
	}

	if conflicts, cerr := s.engine.Conflicts(ctx); cerr == nil {
		data["Conflicts"] = conflicts
	}

	s.render(w, r, "admin_sync", http.StatusOK, data)
}

// handleSyncBind submits Apple ID + password to begin binding.
func (s *Server) handleSyncBind(w http.ResponseWriter, r *http.Request) {
	if !s.syncEnabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	appleID := strings.TrimSpace(r.PostFormValue("apple_id"))
	password := r.PostFormValue("password")
	if appleID == "" || password == "" {
		s.redirectSync(w, r, "creds")
		return
	}

	pending, handle, err := s.engine.BeginBind(ctx, uid, appleID, password)
	if err != nil {
		logError(err)
		s.redirectSync(w, r, "binderr")
		return
	}
	if pending {
		if err := s.setBindHandle(w, r, appleID, handle); err != nil {
			s.serverError(w, err)
			return
		}
		http.Redirect(w, r, "/admin/sync", http.StatusSeeOther)
		return
	}
	s.redirectSync(w, r, "bound")
}

// handleSyncBind2FA submits the 2FA code for a pending bind.
func (s *Server) handleSyncBind2FA(w http.ResponseWriter, r *http.Request) {
	if !s.syncEnabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	_, handle, ok := s.getBindHandle(r)
	if !ok {
		s.redirectSync(w, r, "nohandle")
		return
	}
	code := strings.TrimSpace(r.PostFormValue("code"))
	if err := s.engine.CompleteBind(ctx, uid, handle, code); err != nil {
		logError(err)
		s.redirectSync(w, r, "2faerr")
		return
	}
	s.clearBindHandle(w, r)
	s.redirectSync(w, r, "bound")
}

// handleSyncSetFolder records the chosen root Notes folder.
func (s *Server) handleSyncSetFolder(w http.ResponseWriter, r *http.Request) {
	if !s.syncEnabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	folder := strings.TrimSpace(r.PostFormValue("folder"))
	if folder == "" {
		s.redirectSync(w, r, "nofolder")
		return
	}
	if err := s.engine.SetFolder(ctx, uid, folder); err != nil {
		s.serverError(w, err)
		return
	}
	s.redirectSync(w, r, "folderset")
}

// handleSyncPull triggers an inbound sync.
func (s *Server) handleSyncPull(w http.ResponseWriter, r *http.Request) {
	if !s.syncEnabled() {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	if _, err := s.engine.PullUser(ctx, uid); err != nil {
		logError(err)
		if errors.Is(err, store.ErrNotFound) {
			s.redirectSync(w, r, "needsbind")
			return
		}
		s.redirectSync(w, r, "pullerr")
		return
	}
	s.redirectSync(w, r, "pulled")
}

// handleSyncPush triggers an outbound sync.
func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	if !s.syncEnabled() {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	if _, err := s.engine.PushUser(ctx, uid); err != nil {
		logError(err)
		if errors.Is(err, store.ErrNotFound) {
			s.redirectSync(w, r, "needsbind")
			return
		}
		s.redirectSync(w, r, "pusherr")
		return
	}
	s.redirectSync(w, r, "pushed")
}

// handleSyncResolve resolves a conflict, keeping the local or remote side.
func (s *Server) handleSyncResolve(w http.ResponseWriter, r *http.Request) {
	if !s.syncEnabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	choice := notesync.ResolveKeepRemote
	if r.PostFormValue("choice") == "local" {
		choice = notesync.ResolveKeepLocal
	}
	if err := s.engine.ResolveConflict(ctx, uid, id, choice); err != nil {
		logError(err)
		s.serverError(w, err)
		return
	}
	s.redirectSync(w, r, "resolved")
}

// handleSyncUnbind removes the iCloud binding.
func (s *Server) handleSyncUnbind(w http.ResponseWriter, r *http.Request) {
	if !s.syncEnabled() {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	if err := s.engine.Unbind(ctx, uid); err != nil {
		s.serverError(w, err)
		return
	}
	s.clearBindHandle(w, r)
	s.redirectSync(w, r, "unbound")
}
