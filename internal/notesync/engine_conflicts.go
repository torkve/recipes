package notesync

import "context"

// ConflictView is an unresolved sync conflict enriched for display: the recipe title
// and a diff of the local recipe against the current iCloud note (remote on the "-"
// side, local on the "+" side).
type ConflictView struct {
	ID     int64
	Title  string // recipe title, or "" for non-recipe conflicts (then Detail stands alone)
	Detail string
	Diff   []DiffLine
}

// ConflictsDetailed lists unresolved conflicts enriched with the recipe title and a
// local-vs-remote diff. It fetches the zone once (only when a recipe conflict exists)
// to project the remote side; if that fetch fails it still returns titles and details
// without a diff.
func (e *Engine) ConflictsDetailed(ctx context.Context, userID int64) ([]ConflictView, error) {
	l := e.userLock(userID)
	l.Lock()
	defer l.Unlock()

	conflicts, err := e.store.ListConflicts(ctx)
	if err != nil {
		return nil, err
	}
	if len(conflicts) == 0 {
		return nil, nil
	}

	// Best-effort: pull current remote notes once to diff against (only if needed).
	remoteByID := map[NoteID]Note{}
	for _, c := range conflicts {
		if c.RecipeID == nil {
			continue
		}
		if acct, aerr := e.store.GetICloudAccount(ctx, userID); aerr == nil {
			if sess, serr := e.restoreSession(ctx, acct); serr == nil {
				if _, notes, _, ferr := e.provider.FetchZone(ctx, sess, FolderID(acct.NotesFolder), ""); ferr == nil {
					for _, n := range notes {
						remoteByID[n.ID] = n
					}
				}
			}
		}
		break
	}

	views := make([]ConflictView, 0, len(conflicts))
	for _, c := range conflicts {
		v := ConflictView{ID: c.ID, Detail: c.Detail}
		if c.RecipeID != nil {
			if rec, rerr := e.store.GetRecipe(ctx, *c.RecipeID); rerr == nil {
				v.Title = rec.Title
				local := projectForDiff(rec.Title, rec.Ingredients, rec.StepsHTML)
				var remote []string
				if c.NoteID != nil {
					if n, ok := remoteByID[NoteID(*c.NoteID)]; ok {
						remote = projectForDiff(n.Title, ChecklistsToIngredients(n.Checklists), n.BodyHTML)
					}
				}
				v.Diff = LineDiff(remote, local)
			}
		}
		views = append(views, v)
	}
	return views, nil
}
