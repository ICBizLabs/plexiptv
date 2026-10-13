// Package store persists sources, channels, overrides, rules and devices in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sources(
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	kind TEXT NOT NULL,              -- xtream | m3u | epg
	url TEXT NOT NULL,
	username TEXT NOT NULL DEFAULT '',
	password TEXT NOT NULL DEFAULT '',
	epg_url TEXT NOT NULL DEFAULT '',
	max_conns INTEGER NOT NULL DEFAULT 0,       -- 0 = use provider value (xtream) / unlimited
	provider_conns INTEGER NOT NULL DEFAULT 0,  -- reported by xtream user_info
	stream_mode TEXT NOT NULL DEFAULT 'auto',   -- auto | proxy | ffmpeg | redirect
	user_agent TEXT NOT NULL DEFAULT '',
	refresh_hours INTEGER NOT NULL DEFAULT 24,
	enabled INTEGER NOT NULL DEFAULT 1,
	last_refresh INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	channel_count INTEGER NOT NULL DEFAULT 0,
	skipped_count INTEGER NOT NULL DEFAULT 0,
	epg_count INTEGER NOT NULL DEFAULT 0,
	epg_error TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL DEFAULT 0,
	stream_ext TEXT NOT NULL DEFAULT 'ts'    -- xtream container: ts | m3u8
);
CREATE TABLE IF NOT EXISTS channels(
	id INTEGER PRIMARY KEY,
	source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
	ext_key TEXT NOT NULL,
	name TEXT NOT NULL,
	grp TEXT NOT NULL DEFAULT '',
	logo TEXT NOT NULL DEFAULT '',
	tvg_id TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL DEFAULT '',
	sort INTEGER NOT NULL DEFAULT 0,
	present INTEGER NOT NULL DEFAULT 1,
	gen INTEGER NOT NULL DEFAULT 0,
	UNIQUE(source_id, ext_key)
);
CREATE TABLE IF NOT EXISTS overrides(
	channel_id INTEGER PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
	enabled INTEGER,                 -- NULL = follow rules, 0/1 = manual
	number INTEGER NOT NULL DEFAULT 0,
	name TEXT NOT NULL DEFAULT '',
	logo TEXT NOT NULL DEFAULT '',
	epg_id TEXT NOT NULL DEFAULT '',
	device_id INTEGER NOT NULL DEFAULT 0,
	mode TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS rules(
	id INTEGER PRIMARY KEY,
	position INTEGER NOT NULL DEFAULT 0,
	action TEXT NOT NULL,            -- include | exclude
	field TEXT NOT NULL,             -- group | name | tvg_id | any
	op TEXT NOT NULL,                -- equals | contains | prefix | regex
	value TEXT NOT NULL,
	source_id INTEGER NOT NULL DEFAULT 0,
	device_id INTEGER NOT NULL DEFAULT 0,
	enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS devices(
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	device_id TEXT NOT NULL,
	port INTEGER NOT NULL UNIQUE,
	tuners INTEGER NOT NULL DEFAULT 2,
	start_number INTEGER NOT NULL DEFAULT 1000,
	max_channels INTEGER NOT NULL DEFAULT 480   -- 0 = no limit
);
CREATE TABLE IF NOT EXISTS epg_channels(
	source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
	xmltv_id TEXT NOT NULL,
	name TEXT NOT NULL,
	icon TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(source_id, xmltv_id)
);
CREATE INDEX IF NOT EXISTS channels_source ON channels(source_id, present);
`

type Store struct {
	DB *sql.DB
}

func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "plexiptv.db")
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises writes and avoids SQLITE_BUSY. Reads for the
	// UI are served from the in-memory catalog, so this is not a bottleneck.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{DB: db}, nil
}

// migrate adds columns introduced after a database was first created.
func migrate(db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE sources ADD COLUMN stream_ext TEXT NOT NULL DEFAULT 'ts'`,
		`ALTER TABLE devices ADD COLUMN max_channels INTEGER NOT NULL DEFAULT 480`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.DB.Close() }

// ---- settings ----

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---- sources ----

type Source struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	URL           string `json:"url"`
	Username      string `json:"username"`
	Password      string `json:"password,omitempty"`
	EPGURL        string `json:"epg_url"`
	MaxConns      int    `json:"max_conns"`
	ProviderConns int    `json:"provider_conns"`
	StreamMode    string `json:"stream_mode"`
	UserAgent     string `json:"user_agent"`
	RefreshHours  int    `json:"refresh_hours"`
	Enabled       bool   `json:"enabled"`
	LastRefresh   int64  `json:"last_refresh"`
	LastError     string `json:"last_error"`
	ChannelCount  int    `json:"channel_count"`
	SkippedCount  int    `json:"skipped_count"`
	EPGCount      int    `json:"epg_count"`
	EPGError      string `json:"epg_error"`
	StreamExt     string `json:"stream_ext"`
}

// EffectiveConns is the concurrent-stream limit for this source (0 = unlimited).
func (s Source) EffectiveConns() int {
	if s.MaxConns > 0 {
		return s.MaxConns
	}
	return s.ProviderConns
}

const sourceCols = `id,name,kind,url,username,password,epg_url,max_conns,provider_conns,stream_mode,
	user_agent,refresh_hours,enabled,last_refresh,last_error,channel_count,skipped_count,epg_count,epg_error,stream_ext`

func scanSource(r interface{ Scan(...any) error }) (Source, error) {
	var s Source
	err := r.Scan(&s.ID, &s.Name, &s.Kind, &s.URL, &s.Username, &s.Password, &s.EPGURL, &s.MaxConns,
		&s.ProviderConns, &s.StreamMode, &s.UserAgent, &s.RefreshHours, &s.Enabled, &s.LastRefresh,
		&s.LastError, &s.ChannelCount, &s.SkippedCount, &s.EPGCount, &s.EPGError, &s.StreamExt)
	return s, err
}

func (s *Store) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+sourceCols+` FROM sources ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func (s *Store) Source(ctx context.Context, id int64) (Source, error) {
	return scanSource(s.DB.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM sources WHERE id=?`, id))
}

func (s *Store) SaveSource(ctx context.Context, src *Source) error {
	src.URL = strings.TrimSpace(src.URL)
	if src.ID == 0 {
		res, err := s.DB.ExecContext(ctx, `INSERT INTO sources(name,kind,url,username,password,epg_url,max_conns,
			stream_mode,user_agent,refresh_hours,enabled,position) VALUES(?,?,?,?,?,?,?,?,?,?,?,
			(SELECT COALESCE(MAX(position),0)+1 FROM sources))`,
			src.Name, src.Kind, src.URL, src.Username, src.Password, src.EPGURL, src.MaxConns,
			src.StreamMode, src.UserAgent, src.RefreshHours, src.Enabled)
		if err != nil {
			return err
		}
		src.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE sources SET name=?,kind=?,url=?,username=?,password=?,epg_url=?,
		max_conns=?,stream_mode=?,user_agent=?,refresh_hours=?,enabled=? WHERE id=?`,
		src.Name, src.Kind, src.URL, src.Username, src.Password, src.EPGURL, src.MaxConns,
		src.StreamMode, src.UserAgent, src.RefreshHours, src.Enabled, src.ID)
	return err
}

func (s *Store) DeleteSource(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id)
	return err
}

func (s *Store) SetSourceStatus(ctx context.Context, id int64, refreshed int64, lastErr string, count, skipped, providerConns int) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sources SET last_refresh=?,last_error=?,channel_count=?,skipped_count=?,
		provider_conns=CASE WHEN ?>0 THEN ? ELSE provider_conns END WHERE id=?`,
		refreshed, lastErr, count, skipped, providerConns, providerConns, id)
	return err
}

func (s *Store) SetSourceError(ctx context.Context, id int64, refreshed int64, lastErr string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sources SET last_refresh=?,last_error=? WHERE id=?`, refreshed, lastErr, id)
	return err
}

func (s *Store) SetStreamExt(ctx context.Context, id int64, ext string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sources SET stream_ext=? WHERE id=?`, ext, id)
	return err
}

func (s *Store) SetEPGStatus(ctx context.Context, id int64, count int, epgErr string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sources SET epg_count=?,epg_error=? WHERE id=?`, count, epgErr, id)
	return err
}

// ---- channels ----

type Channel struct {
	ID       int64
	SourceID int64
	ExtKey   string
	Name     string
	Group    string
	Logo     string
	TvgID    string
	URL      string
	Sort     int
	Present  bool
}

func (s *Store) AllChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id,c.source_id,c.ext_key,c.name,c.grp,c.logo,c.tvg_id,c.url,c.sort,c.present
		FROM channels c JOIN sources s ON s.id=c.source_id ORDER BY s.position, s.id, c.sort, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Channel, 0, 4096)
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.SourceID, &c.ExtKey, &c.Name, &c.Group, &c.Logo, &c.TvgID, &c.URL, &c.Sort, &c.Present); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Importer upserts channels for one source inside a single transaction.
// Channels not seen during the import are marked not-present (never deleted),
// so their numbers and overrides survive a provider hiccup.
type Importer struct {
	tx       *sql.Tx
	stmt     *sql.Stmt
	sourceID int64
	gen      int64
	sort     int
	Count    int
}

func (s *Store) BeginImport(ctx context.Context, sourceID, gen int64) (*Importer, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO channels(source_id,ext_key,name,grp,logo,tvg_id,url,sort,present,gen)
		VALUES(?,?,?,?,?,?,?,?,1,?)
		ON CONFLICT(source_id,ext_key) DO UPDATE SET name=excluded.name,grp=excluded.grp,logo=excluded.logo,
		tvg_id=excluded.tvg_id,url=excluded.url,sort=excluded.sort,present=1,gen=excluded.gen`)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return &Importer{tx: tx, stmt: stmt, sourceID: sourceID, gen: gen}, nil
}

func (im *Importer) Add(ctx context.Context, c Channel) error {
	im.sort++
	im.Count++
	_, err := im.stmt.ExecContext(ctx, im.sourceID, c.ExtKey, c.Name, c.Group, c.Logo, c.TvgID, c.URL, im.sort, im.gen)
	return err
}

func (im *Importer) Commit(ctx context.Context) error {
	im.stmt.Close()
	if _, err := im.tx.ExecContext(ctx, `UPDATE channels SET present=0 WHERE source_id=? AND gen<>?`, im.sourceID, im.gen); err != nil {
		im.tx.Rollback()
		return err
	}
	return im.tx.Commit()
}

func (im *Importer) Rollback() {
	im.stmt.Close()
	im.tx.Rollback()
}

// ---- overrides ----

type Override struct {
	ChannelID int64
	Enabled   *bool // nil = follow rules
	Number    int
	Name      string
	Logo      string
	EPGID     string
	DeviceID  int64
	Mode      string
}

func (o Override) IsZero() bool {
	return o.Enabled == nil && o.Number == 0 && o.Name == "" && o.Logo == "" && o.EPGID == "" && o.DeviceID == 0 && o.Mode == ""
}

func (s *Store) Overrides(ctx context.Context) (map[int64]Override, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT channel_id,enabled,number,name,logo,epg_id,device_id,mode FROM overrides`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Override{}
	for rows.Next() {
		var o Override
		var en sql.NullBool
		if err := rows.Scan(&o.ChannelID, &en, &o.Number, &o.Name, &o.Logo, &o.EPGID, &o.DeviceID, &o.Mode); err != nil {
			return nil, err
		}
		if en.Valid {
			v := en.Bool
			o.Enabled = &v
		}
		out[o.ChannelID] = o
	}
	return out, rows.Err()
}

func (s *Store) SaveOverrides(ctx context.Context, list []Override) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	up, err := tx.PrepareContext(ctx, `INSERT INTO overrides(channel_id,enabled,number,name,logo,epg_id,device_id,mode)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(channel_id) DO UPDATE SET enabled=excluded.enabled,number=excluded.number,
		name=excluded.name,logo=excluded.logo,epg_id=excluded.epg_id,device_id=excluded.device_id,mode=excluded.mode`)
	if err != nil {
		return err
	}
	defer up.Close()
	del, err := tx.PrepareContext(ctx, `DELETE FROM overrides WHERE channel_id=?`)
	if err != nil {
		return err
	}
	defer del.Close()
	for _, o := range list {
		if o.IsZero() {
			if _, err := del.ExecContext(ctx, o.ChannelID); err != nil {
				return err
			}
			continue
		}
		var en any
		if o.Enabled != nil {
			en = *o.Enabled
		}
		if _, err := up.ExecContext(ctx, o.ChannelID, en, o.Number, o.Name, o.Logo, o.EPGID, o.DeviceID, o.Mode); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- rules ----

type Rule struct {
	ID       int64  `json:"id"`
	Position int    `json:"position"`
	Action   string `json:"action"`
	Field    string `json:"field"`
	Op       string `json:"op"`
	Value    string `json:"value"`
	SourceID int64  `json:"source_id"`
	DeviceID int64  `json:"device_id"`
	Enabled  bool   `json:"enabled"`
}

func (s *Store) Rules(ctx context.Context) ([]Rule, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,position,action,field,op,value,source_id,device_id,enabled FROM rules ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.Position, &r.Action, &r.Field, &r.Op, &r.Value, &r.SourceID, &r.DeviceID, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SaveRule(ctx context.Context, r *Rule) error {
	if r.ID == 0 {
		res, err := s.DB.ExecContext(ctx, `INSERT INTO rules(position,action,field,op,value,source_id,device_id,enabled)
			VALUES((SELECT COALESCE(MAX(position),0)+1 FROM rules),?,?,?,?,?,?,?)`,
			r.Action, r.Field, r.Op, r.Value, r.SourceID, r.DeviceID, r.Enabled)
		if err != nil {
			return err
		}
		r.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE rules SET action=?,field=?,op=?,value=?,source_id=?,device_id=?,enabled=? WHERE id=?`,
		r.Action, r.Field, r.Op, r.Value, r.SourceID, r.DeviceID, r.Enabled, r.ID)
	return err
}

func (s *Store) DeleteRule(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM rules WHERE id=?`, id)
	return err
}

func (s *Store) ReorderRules(ctx context.Context, ids []int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE rules SET position=? WHERE id=?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- devices ----

type Device struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DeviceID    string `json:"device_id"`
	Port        int    `json:"port"`
	Tuners      int    `json:"tuners"`
	StartNumber int    `json:"start_number"`
	MaxChannels int    `json:"max_channels"` // 0 = no limit
}

func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,device_id,port,tuners,start_number,max_channels FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.DeviceID, &d.Port, &d.Tuners, &d.StartNumber, &d.MaxChannels); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SaveDevice(ctx context.Context, d *Device) error {
	if d.ID == 0 {
		res, err := s.DB.ExecContext(ctx, `INSERT INTO devices(name,device_id,port,tuners,start_number,max_channels) VALUES(?,?,?,?,?,?)`,
			d.Name, d.DeviceID, d.Port, d.Tuners, d.StartNumber, d.MaxChannels)
		if err != nil {
			return err
		}
		d.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET name=?,port=?,tuners=?,start_number=?,max_channels=? WHERE id=?`,
		d.Name, d.Port, d.Tuners, d.StartNumber, d.MaxChannels, d.ID)
	return err
}

func (s *Store) DeleteDevice(ctx context.Context, id int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`UPDATE overrides SET device_id=0 WHERE device_id=?`,
		`UPDATE rules SET device_id=0 WHERE device_id=?`,
		`DELETE FROM devices WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- EPG channel index ----

type EPGChannel struct {
	SourceID int64  `json:"source_id"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Icon     string `json:"icon"`
}

func (s *Store) ReplaceEPGChannels(ctx context.Context, sourceID int64, list []EPGChannel) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM epg_channels WHERE source_id=?`, sourceID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO epg_channels(source_id,xmltv_id,name,icon) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, c := range list {
		if _, err := stmt.ExecContext(ctx, sourceID, c.ID, c.Name, c.Icon); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) EPGChannels(ctx context.Context) ([]EPGChannel, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT e.source_id,e.xmltv_id,e.name,e.icon FROM epg_channels e
		JOIN sources s ON s.id=e.source_id ORDER BY s.position, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EPGChannel
	for rows.Next() {
		var c EPGChannel
		if err := rows.Scan(&c.SourceID, &c.ID, &c.Name, &c.Icon); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
