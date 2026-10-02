package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Metadata, boards and board items: what people add on top of the registry
// (owners, notes, plans). It lives in its own tables, which a registry import
// (PUT /requirements) never touches, so it survives every re-import.

// Metadata object kinds. Keys: spec "ENG-465"; criterion and gap
// "ENG-465:AC-06"; test the registry test key.
const (
	MetaSpec      = "spec"
	MetaCriterion = "criterion"
	MetaTest      = "test"
	MetaGap       = "gap"
)

var metaKinds = map[string]bool{MetaSpec: true, MetaCriterion: true, MetaTest: true, MetaGap: true}

var emptyObject = json.RawMessage(`{}`)

// userOf is who made a write: the X-Olly-User header, free text, may be empty.
func userOf(c *gin.Context) string { return strings.TrimSpace(c.GetHeader("X-Olly-User")) }

// asObject checks raw is a JSON object (null or empty reads as {}) and
// returns it decoded.
func asObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return map[string]json.RawMessage{}, nil
	}
	if t[0] != '{' {
		return nil, errors.New("data must be a JSON object")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(t, &m); err != nil {
		return nil, fmt.Errorf("data is not valid JSON: %w", err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// mergeObject applies patch onto base, shallowly: a key set to null is
// removed, any other value replaces the old one.
func mergeObject(base, patch map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func encodeObject(m map[string]json.RawMessage) []byte {
	if len(m) == 0 {
		return []byte(`{}`)
	}
	b, _ := json.Marshal(m)
	return b
}

// rawOrEmpty is a stored JSON value, or {} when there is none.
func rawOrEmpty(b []byte) json.RawMessage {
	if len(bytes.TrimSpace(b)) == 0 {
		return emptyObject
	}
	return json.RawMessage(b)
}

// lockClause takes a row lock where the database has one (Postgres), so two
// concurrent PATCHes do not lose each other's keys.
func lockClause(tx *gorm.DB) string {
	if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
		return " FOR UPDATE"
	}
	return ""
}

// --- metadata ---------------------------------------------------------------

type metaRecord struct {
	Kind      string          `json:"kind"`
	Key       string          `json:"key"`
	Data      json.RawMessage `json:"data"`
	UpdatedAt *time.Time      `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

type metaRow struct {
	ObjKind   string
	ObjKey    string
	Data      []byte
	UpdatedAt *time.Time
	UpdatedBy string
}

func (r metaRow) record() metaRecord {
	return metaRecord{Kind: r.ObjKind, Key: r.ObjKey, Data: rawOrEmpty(r.Data), UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy}
}

// metaIndex is every stored metadata object, by kind and key.
type metaIndex map[string]json.RawMessage

func (m metaIndex) get(kind, key string) json.RawMessage {
	if v, ok := m[kind+"\x00"+key]; ok {
		return v
	}
	return emptyObject
}

func (h *CoverageHandler) loadAllMeta() (metaIndex, error) {
	var rows []metaRow
	if err := h.db.Raw(`SELECT obj_kind, obj_key, data FROM requirement_meta`).Scan(&rows).Error; err != nil {
		return nil, err
	}
	idx := make(metaIndex, len(rows))
	for _, r := range rows {
		idx[r.ObjKind+"\x00"+r.ObjKey] = rawOrEmpty(r.Data)
	}
	return idx, nil
}

func (h *CoverageHandler) registerMetaRoutes(g *gin.RouterGroup) {
	// gin has no literal colon in paths: "/meta:op" is a wildcard on the end
	// of the "meta" segment, whose value is the rest (":bulk").
	g.POST("/meta:op", func(c *gin.Context) {
		if c.Param("op") != ":bulk" {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		h.bulkMeta(c)
	})
	g.GET("/meta/:kind", h.listMeta)
	g.GET("/meta/:kind/*key", h.getMeta)
	g.PUT("/meta/:kind/*key", h.putMeta)
	g.PATCH("/meta/:kind/*key", h.patchMeta)
}

// metaTarget reads and checks the kind and key from the path. The key is a
// catch-all because test keys hold slashes.
func metaTarget(c *gin.Context) (string, string, bool) {
	kind := c.Param("kind")
	key := strings.TrimPrefix(c.Param("key"), "/")
	if !metaKinds[kind] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown kind " + kind + " (want spec, criterion, test or gap)"})
		return "", "", false
	}
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing key"})
		return "", "", false
	}
	return kind, key, true
}

func (h *CoverageHandler) readMeta(tx *gorm.DB, kind, key string, lock bool) (*metaRow, error) {
	q := `SELECT obj_kind, obj_key, data, updated_at, updated_by FROM requirement_meta WHERE obj_kind = ? AND obj_key = ?`
	if lock {
		q += lockClause(tx)
	}
	var rows []metaRow
	if err := tx.Raw(q, kind, key).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// writeMeta stores one object: merge applies data onto what is stored (null
// deletes a key), otherwise data replaces it.
func writeMeta(tx *gorm.DB, kind, key string, data map[string]json.RawMessage, merge bool, user string, now time.Time) (metaRow, error) {
	if merge {
		q := `SELECT obj_kind, obj_key, data, updated_at, updated_by FROM requirement_meta WHERE obj_kind = ? AND obj_key = ?` + lockClause(tx)
		var rows []metaRow
		if err := tx.Raw(q, kind, key).Scan(&rows).Error; err != nil {
			return metaRow{}, err
		}
		base := map[string]json.RawMessage{}
		if len(rows) > 0 {
			if b, err := asObject(rows[0].Data); err == nil {
				base = b
			}
		}
		data = mergeObject(base, data)
	}
	body := encodeObject(data)
	err := tx.Exec(`INSERT INTO requirement_meta (obj_kind, obj_key, data, updated_at, updated_by) VALUES (?,?,?,?,?)
		ON CONFLICT (obj_kind, obj_key) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		kind, key, body, now, user).Error
	return metaRow{ObjKind: kind, ObjKey: key, Data: body, UpdatedAt: &now, UpdatedBy: user}, err
}

func (h *CoverageHandler) getMeta(c *gin.Context) {
	kind, key, ok := metaTarget(c)
	if !ok {
		return
	}
	r, err := h.readMeta(h.db, kind, key, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if r == nil {
		// Nothing stored reads as an empty object, as in the coverage response.
		c.JSON(http.StatusOK, metaRecord{Kind: kind, Key: key, Data: emptyObject})
		return
	}
	c.JSON(http.StatusOK, r.record())
}

func (h *CoverageHandler) listMeta(c *gin.Context) {
	kind := c.Param("kind")
	if !metaKinds[kind] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown kind " + kind + " (want spec, criterion, test or gap)"})
		return
	}
	var rows []metaRow
	if err := h.db.Raw(`SELECT obj_kind, obj_key, data, updated_at, updated_by FROM requirement_meta WHERE obj_kind = ? ORDER BY obj_key`, kind).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]metaRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.record())
	}
	c.JSON(http.StatusOK, out)
}

type metaBody struct {
	Data json.RawMessage `json:"data"`
}

func (h *CoverageHandler) writeMetaRoute(c *gin.Context, merge bool) {
	kind, key, ok := metaTarget(c)
	if !ok {
		return
	}
	var body metaBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	data, err := asObject(body.Data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var out metaRow
	err = h.db.Transaction(func(tx *gorm.DB) error {
		var e error
		out, e = writeMeta(tx, kind, key, data, merge, userOf(c), time.Now().UTC())
		return e
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out.record())
}

func (h *CoverageHandler) putMeta(c *gin.Context)   { h.writeMetaRoute(c, false) }
func (h *CoverageHandler) patchMeta(c *gin.Context) { h.writeMetaRoute(c, true) }

type bulkMetaItem struct {
	Kind string          `json:"kind"`
	Key  string          `json:"key"`
	Data json.RawMessage `json:"data"`
	Mode string          `json:"mode"` // merge (default) | replace
}

// bulkMeta writes many objects in one transaction: all or none. Every item
// is checked before anything is written.
func (h *CoverageHandler) bulkMeta(c *gin.Context) {
	var body struct {
		Items []bulkMetaItem `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	datas := make([]map[string]json.RawMessage, len(body.Items))
	for i, it := range body.Items {
		where := "item " + strconv.Itoa(i) + ": "
		if !metaKinds[it.Kind] {
			c.JSON(http.StatusBadRequest, gin.H{"error": where + "unknown kind " + it.Kind})
			return
		}
		if it.Key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": where + "missing key"})
			return
		}
		if it.Mode != "" && it.Mode != "merge" && it.Mode != "replace" {
			c.JSON(http.StatusBadRequest, gin.H{"error": where + "mode must be merge or replace"})
			return
		}
		d, err := asObject(it.Data)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": where + err.Error()})
			return
		}
		datas[i] = d
	}
	user, now := userOf(c), time.Now().UTC()
	out := make([]metaRecord, 0, len(body.Items))
	err := h.db.Transaction(func(tx *gorm.DB) error {
		for i, it := range body.Items {
			r, err := writeMeta(tx, it.Kind, it.Key, datas[i], it.Mode != "replace", user, now)
			if err != nil {
				return err
			}
			out = append(out, r.record())
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"updated": len(out), "items": out})
}

// --- boards -----------------------------------------------------------------

var boardKinds = map[string]bool{"buckets": true, "gantt": true}

type board struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Definition json.RawMessage `json:"definition"`
	CreatedAt  *time.Time      `json:"created_at"`
	UpdatedAt  *time.Time      `json:"updated_at"`
	UpdatedBy  string          `json:"updated_by"`
}

type boardRow struct {
	ID         int64
	Name       string
	Kind       string
	Definition []byte
	CreatedAt  *time.Time
	UpdatedAt  *time.Time
	UpdatedBy  string
}

func (r boardRow) board() board {
	return board{ID: r.ID, Name: r.Name, Kind: r.Kind, Definition: rawOrEmpty(r.Definition), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy}
}

func (h *CoverageHandler) registerBoardRoutes(g *gin.RouterGroup) {
	g.GET("/boards", h.listBoards)
	g.POST("/boards", h.createBoard)
	g.GET("/boards/:id", h.getBoard)
	g.PUT("/boards/:id", h.putBoard)
	g.DELETE("/boards/:id", h.deleteBoard)
	g.GET("/boards/:id/items", h.listItems)
	g.POST("/boards/:id/items", h.createItem)
	g.GET("/boards/:id/items/:item", h.getItem)
	g.PUT("/boards/:id/items/:item", h.putItem)
	g.PATCH("/boards/:id/items/:item", h.patchItem)
	g.DELETE("/boards/:id/items/:item", h.deleteItem)
}

type boardBody struct {
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Definition json.RawMessage `json:"definition"`
}

func (b *boardBody) check() ([]byte, error) {
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return nil, errors.New("name is required")
	}
	if !boardKinds[b.Kind] {
		return nil, errors.New("kind must be buckets or gantt")
	}
	def, err := asObject(b.Definition)
	if err != nil {
		return nil, errors.New("definition must be a JSON object")
	}
	// Keep the definition exactly as sent (key order, numbers) when it is one.
	if t := bytes.TrimSpace(b.Definition); len(t) > 0 && t[0] == '{' {
		return t, nil
	}
	return encodeObject(def), nil
}

func pathID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad " + name})
		return 0, false
	}
	return id, true
}

func (h *CoverageHandler) findBoard(tx *gorm.DB, id int64) (*boardRow, error) {
	var rows []boardRow
	if err := tx.Raw(`SELECT id, name, kind, definition, created_at, updated_at, updated_by FROM coverage_boards WHERE id = ?`, id).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// boardOr404 loads the board in the path, answering 400/404/500 itself.
func (h *CoverageHandler) boardOr404(c *gin.Context) (*boardRow, bool) {
	id, ok := pathID(c, "id")
	if !ok {
		return nil, false
	}
	b, err := h.findBoard(h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return nil, false
	}
	if b == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no board " + c.Param("id")})
		return nil, false
	}
	return b, true
}

func (h *CoverageHandler) listBoards(c *gin.Context) {
	var rows []boardRow
	if err := h.db.Raw(`SELECT id, name, kind, definition, created_at, updated_at, updated_by FROM coverage_boards ORDER BY id`).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]board, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.board())
	}
	c.JSON(http.StatusOK, out)
}

func (h *CoverageHandler) createBoard(c *gin.Context) {
	var body boardBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	def, err := body.check()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	var id int64
	if err := h.db.Raw(`INSERT INTO coverage_boards (name, kind, definition, created_at, updated_at, updated_by) VALUES (?,?,?,?,?,?) RETURNING id`,
		body.Name, body.Kind, def, now, now, userOf(c)).Scan(&id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, board{ID: id, Name: body.Name, Kind: body.Kind, Definition: def, CreatedAt: &now, UpdatedAt: &now, UpdatedBy: userOf(c)})
}

func (h *CoverageHandler) getBoard(c *gin.Context) {
	if b, ok := h.boardOr404(c); ok {
		c.JSON(http.StatusOK, b.board())
	}
}

func (h *CoverageHandler) putBoard(c *gin.Context) {
	b, ok := h.boardOr404(c)
	if !ok {
		return
	}
	var body boardBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	def, err := body.check()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	if err := h.db.Exec(`UPDATE coverage_boards SET name = ?, kind = ?, definition = ?, updated_at = ?, updated_by = ? WHERE id = ?`,
		body.Name, body.Kind, def, now, userOf(c), b.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, board{ID: b.ID, Name: body.Name, Kind: body.Kind, Definition: def, CreatedAt: b.CreatedAt, UpdatedAt: &now, UpdatedBy: userOf(c)})
}

// deleteBoard removes the board and every item on it.
func (h *CoverageHandler) deleteBoard(c *gin.Context) {
	b, ok := h.boardOr404(c)
	if !ok {
		return
	}
	var n int64
	err := h.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(`DELETE FROM coverage_board_items WHERE board_id = ?`, b.ID)
		if res.Error != nil {
			return res.Error
		}
		n = res.RowsAffected
		return tx.Exec(`DELETE FROM coverage_boards WHERE id = ?`, b.ID).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": b.ID, "items_deleted": n})
}

// --- board items --------------------------------------------------------------

type itemRef struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

type boardItem struct {
	ID        int64           `json:"id"`
	BoardID   int64           `json:"board_id"`
	Title     string          `json:"title"`
	Start     string          `json:"start"`
	End       string          `json:"end"`
	DependsOn []int64         `json:"depends_on"`
	Bucket    string          `json:"bucket"`
	Refs      []itemRef       `json:"refs"`
	Metadata  json.RawMessage `json:"metadata"`
	Sort      float64         `json:"sort"`
	CreatedAt *time.Time      `json:"created_at"`
	UpdatedAt *time.Time      `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

type itemRow struct {
	ID        int64
	BoardID   int64
	Title     string
	StartDate string
	EndDate   string
	DependsOn []byte
	Bucket    string
	Refs      []byte
	Metadata  []byte
	Sort      float64
	CreatedAt *time.Time
	UpdatedAt *time.Time
	UpdatedBy string
}

func (r itemRow) item() boardItem {
	it := boardItem{ID: r.ID, BoardID: r.BoardID, Title: r.Title, Start: r.StartDate, End: r.EndDate, Bucket: r.Bucket,
		Metadata: rawOrEmpty(r.Metadata), Sort: r.Sort, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy,
		DependsOn: []int64{}, Refs: []itemRef{}}
	_ = json.Unmarshal(r.DependsOn, &it.DependsOn)
	_ = json.Unmarshal(r.Refs, &it.Refs)
	if it.DependsOn == nil {
		it.DependsOn = []int64{}
	}
	if it.Refs == nil {
		it.Refs = []itemRef{}
	}
	return it
}

const itemCols = `id, board_id, title, start_date, end_date, depends_on, bucket, refs, metadata, sort, created_at, updated_at, updated_by`

func (h *CoverageHandler) boardItems(tx *gorm.DB, boardID int64) ([]itemRow, error) {
	var rows []itemRow
	err := tx.Raw(`SELECT `+itemCols+` FROM coverage_board_items WHERE board_id = ? ORDER BY sort, id`, boardID).Scan(&rows).Error
	return rows, err
}

// checkItem validates an item against its board: dates are YYYY-MM-DD or
// empty and end is not before start, dependencies are other items of the same
// board, refs name a metadata kind and a key.
func checkItem(it *boardItem, siblings []itemRow) error {
	it.Title = strings.TrimSpace(it.Title)
	var start, end time.Time
	for _, d := range []struct {
		name, v string
		t       *time.Time
	}{{"start", it.Start, &start}, {"end", it.End, &end}} {
		if d.v == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", d.v)
		if err != nil {
			return fmt.Errorf("%s must be a date (YYYY-MM-DD) or empty", d.name)
		}
		*d.t = t
	}
	if it.Start != "" && it.End != "" && end.Before(start) {
		return errors.New("end is before start")
	}
	known := map[int64]bool{}
	for _, s := range siblings {
		known[s.ID] = true
	}
	seen := map[int64]bool{}
	deps := make([]int64, 0, len(it.DependsOn))
	for _, d := range it.DependsOn {
		if d == it.ID && it.ID != 0 {
			return errors.New("an item cannot depend on itself")
		}
		if !known[d] {
			return fmt.Errorf("depends_on %d is not an item on this board", d)
		}
		if !seen[d] {
			seen[d] = true
			deps = append(deps, d)
		}
	}
	it.DependsOn = deps
	for _, r := range it.Refs {
		if !metaKinds[r.Kind] || r.Key == "" {
			return fmt.Errorf("ref %s/%s must name a kind (spec, criterion, test or gap) and a key", r.Kind, r.Key)
		}
	}
	if it.Refs == nil {
		it.Refs = []itemRef{}
	}
	if _, err := asObject(it.Metadata); err != nil {
		return errors.New("metadata must be a JSON object")
	}
	if len(bytes.TrimSpace(it.Metadata)) == 0 || bytes.Equal(bytes.TrimSpace(it.Metadata), []byte("null")) {
		it.Metadata = emptyObject
	}
	return nil
}

func (h *CoverageHandler) listItems(c *gin.Context) {
	b, ok := h.boardOr404(c)
	if !ok {
		return
	}
	rows, err := h.boardItems(h.db, b.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]boardItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.item())
	}
	c.JSON(http.StatusOK, out)
}

func (h *CoverageHandler) createItem(c *gin.Context) {
	b, ok := h.boardOr404(c)
	if !ok {
		return
	}
	var it boardItem
	if err := c.ShouldBindJSON(&it); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	it.ID, it.BoardID = 0, b.ID
	siblings, err := h.boardItems(h.db, b.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := checkItem(&it, siblings); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	deps, _ := json.Marshal(it.DependsOn)
	refs, _ := json.Marshal(it.Refs)
	if err := h.db.Raw(`INSERT INTO coverage_board_items (board_id, title, start_date, end_date, depends_on, bucket, refs, metadata, sort, created_at, updated_at, updated_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`, b.ID, it.Title, it.Start, it.End, deps, it.Bucket, refs, []byte(it.Metadata), it.Sort, now, now, userOf(c)).
		Scan(&it.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	it.CreatedAt, it.UpdatedAt, it.UpdatedBy = &now, &now, userOf(c)
	c.JSON(http.StatusCreated, it)
}

// itemOr404 loads the board and the item in the path; the item must be on
// that board.
func (h *CoverageHandler) itemOr404(c *gin.Context) (*boardRow, *itemRow, []itemRow, bool) {
	b, ok := h.boardOr404(c)
	if !ok {
		return nil, nil, nil, false
	}
	id, ok := pathID(c, "item")
	if !ok {
		return nil, nil, nil, false
	}
	rows, err := h.boardItems(h.db, b.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return nil, nil, nil, false
	}
	for i := range rows {
		if rows[i].ID == id {
			return b, &rows[i], rows, true
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "no item " + c.Param("item") + " on board " + c.Param("id")})
	return nil, nil, nil, false
}

func (h *CoverageHandler) getItem(c *gin.Context) {
	if _, r, _, ok := h.itemOr404(c); ok {
		c.JSON(http.StatusOK, r.item())
	}
}

func (h *CoverageHandler) saveItem(c *gin.Context, it boardItem, siblings []itemRow, createdAt *time.Time) {
	if err := checkItem(&it, siblings); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	deps, _ := json.Marshal(it.DependsOn)
	refs, _ := json.Marshal(it.Refs)
	if err := h.db.Exec(`UPDATE coverage_board_items SET title = ?, start_date = ?, end_date = ?, depends_on = ?, bucket = ?, refs = ?, metadata = ?, sort = ?,
		updated_at = ?, updated_by = ? WHERE id = ? AND board_id = ?`, it.Title, it.Start, it.End, deps, it.Bucket, refs, []byte(it.Metadata), it.Sort,
		now, userOf(c), it.ID, it.BoardID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	it.CreatedAt, it.UpdatedAt, it.UpdatedBy = createdAt, &now, userOf(c)
	c.JSON(http.StatusOK, it)
}

// putItem replaces every field of the item.
func (h *CoverageHandler) putItem(c *gin.Context) {
	b, cur, siblings, ok := h.itemOr404(c)
	if !ok {
		return
	}
	var it boardItem
	if err := c.ShouldBindJSON(&it); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	it.ID, it.BoardID = cur.ID, b.ID
	h.saveItem(c, it, siblings, cur.CreatedAt)
}

// patchItem changes only the fields sent; metadata is merged shallowly (a
// key set to null is removed).
func (h *CoverageHandler) patchItem(c *gin.Context) {
	b, cur, siblings, ok := h.itemOr404(c)
	if !ok {
		return
	}
	var patch map[string]json.RawMessage
	if err := c.ShouldBindJSON(&patch); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	it := cur.item()
	it.ID, it.BoardID = cur.ID, b.ID
	for k, v := range patch {
		var err error
		switch k {
		case "title":
			err = json.Unmarshal(v, &it.Title)
		case "start":
			err = json.Unmarshal(v, &it.Start)
		case "end":
			err = json.Unmarshal(v, &it.End)
		case "bucket":
			err = json.Unmarshal(v, &it.Bucket)
		case "sort":
			err = json.Unmarshal(v, &it.Sort)
		case "depends_on":
			it.DependsOn = nil
			err = json.Unmarshal(v, &it.DependsOn)
		case "refs":
			it.Refs = nil
			err = json.Unmarshal(v, &it.Refs)
		case "metadata":
			var base, p map[string]json.RawMessage
			if base, err = asObject(it.Metadata); err == nil {
				if p, err = asObject(v); err == nil {
					it.Metadata = encodeObject(mergeObject(base, p))
				}
			}
		case "id", "board_id", "created_at", "updated_at", "updated_by":
			// read-only, ignored
		default:
			err = errors.New("unknown field")
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": k + ": " + err.Error()})
			return
		}
	}
	if it.DependsOn == nil {
		it.DependsOn = []int64{}
	}
	h.saveItem(c, it, siblings, cur.CreatedAt)
}

// deleteItem removes the item and drops it from the depends_on of the rest.
func (h *CoverageHandler) deleteItem(c *gin.Context) {
	b, cur, siblings, ok := h.itemOr404(c)
	if !ok {
		return
	}
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM coverage_board_items WHERE id = ? AND board_id = ?`, cur.ID, b.ID).Error; err != nil {
			return err
		}
		for _, s := range siblings {
			if s.ID == cur.ID {
				continue
			}
			it := s.item()
			kept := make([]int64, 0, len(it.DependsOn))
			for _, d := range it.DependsOn {
				if d != cur.ID {
					kept = append(kept, d)
				}
			}
			if len(kept) != len(it.DependsOn) {
				deps, _ := json.Marshal(kept)
				if err := tx.Exec(`UPDATE coverage_board_items SET depends_on = ? WHERE id = ?`, deps, s.ID).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": cur.ID})
}
