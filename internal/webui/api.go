// Package webui serves the browser client and the JSON it reads.
//
// The UI is a thin view over state the server already owns. It deliberately holds no keys and makes
// no signing decisions: a browser tab is not a place to keep a private key, and the agent already
// exists to hold one. Where the UI needs a signature it says so and points at the agent.
package webui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/cmurray/brc100-poker/internal/game/cards"
	"github.com/cmurray/brc100-poker/internal/game/engine"
	"github.com/cmurray/brc100-poker/internal/protocol/table"
)

//go:embed static
var staticFiles embed.FS

// TableView is one table as the browser renders it.
type TableView struct {
	TableID string `json:"tableId"`
	Phase   string `json:"phase"`

	Seats            int    `json:"seats"`
	BuyInSatoshis    uint64 `json:"buyInSatoshis"`
	SmallBlind       uint64 `json:"smallBlind"`
	BigBlind         uint64 `json:"bigBlind"`
	RefundLockHeight uint32 `json:"refundLockHeight"`

	Players []PlayerView `json:"players"`
	Board   []string     `json:"board"`
	Pot     int64        `json:"pot"`
	ToAct   int          `json:"toAct"`

	// Street is the betting round, or empty before the deal.
	Street string `json:"street,omitempty"`
	// StalledSeat and StallReason surface a stall rather than hiding it behind a spinner.
	StalledSeat int    `json:"stalledSeat"`
	StallReason string `json:"stallReason,omitempty"`

	UpdatedAt time.Time `json:"updatedAt"`
}

// PlayerView is one seat.
type PlayerView struct {
	Seat int `json:"seat"`
	// IdentityKey is truncated: the full key is public, but a UI does not need 66 characters
	// of it to identify a seat.
	IdentityKey string `json:"identityKey"`
	Stack       int64  `json:"stack"`
	Committed   int64  `json:"committed"`
	Folded      bool   `json:"folded"`
	AllIn       bool   `json:"allIn"`
	Funded      bool   `json:"funded"`
	RefundHeld  bool   `json:"refundHeld"`
	// Hole is this seat's cards, and is populated ONLY for the requesting seat. Another
	// seat's cards are never sent, because a browser cannot be trusted not to show them.
	Hole []string `json:"hole,omitempty"`
	// MoneySummary is the player-facing sentence about their own funds.
	MoneySummary string `json:"moneySummary,omitempty"`
	// AtRisk reports whether this seat has value committed with no confirmed outcome.
	AtRisk bool `json:"atRisk"`
}

// Store holds the views the UI reads.
//
// The server is the only writer. A browser cannot mutate game state through this API — it renders
// what it is told and asks the player's agent for anything requiring a key.
type Store struct {
	mu     sync.RWMutex
	tables map[string]*TableView
	// hole holds each seat's cards per table, released to that seat only.
	hole map[string]map[int][]string
	// live is the playable table, if one exists.
	live *LiveTable
	now  func() time.Time
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		tables: make(map[string]*TableView),
		hole:   make(map[string]map[int][]string),
		now:    time.Now,
	}
}

// PutTable records a table's public state.
func (s *Store) PutTable(v TableView) error {
	if v.TableID == "" {
		return errors.New("webui: a table view needs a table id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v.UpdatedAt = s.now()
	s.tables[v.TableID] = &v
	return nil
}

// SetHole records a seat's cards, which are released only to that seat.
func (s *Store) SetHole(tableID string, seat int, hole []cards.Card) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hole[tableID] == nil {
		s.hole[tableID] = make(map[int][]string)
	}
	out := make([]string, 0, len(hole))
	for _, c := range hole {
		out = append(out, c.String())
	}
	s.hole[tableID][seat] = out
}

// Table returns a table's view for a given seat.
//
// seat < 0 means an observer: no hole cards are included for anyone. This is the whole reason the
// API takes a seat rather than returning one blob — a spectator view and a player view are
// different documents, not the same document filtered in the browser.
func (s *Store) Table(tableID string, seat int) (TableView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	v, ok := s.tables[tableID]
	if !ok {
		return TableView{}, fmt.Errorf("webui: no table %q", tableID)
	}

	out := *v
	out.Players = make([]PlayerView, len(v.Players))
	copy(out.Players, v.Players)
	// Strip every hole card, then add back only the requesting seat's.
	for i := range out.Players {
		out.Players[i].Hole = nil
	}
	if seat >= 0 {
		if byTable, ok := s.hole[tableID]; ok {
			if h, ok := byTable[seat]; ok {
				for i := range out.Players {
					if out.Players[i].Seat == seat {
						out.Players[i].Hole = h
					}
				}
			}
		}
	}
	return out, nil
}

// Tables lists every table, without hole cards.
func (s *Store) Tables() []TableView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]TableView, 0, len(s.tables))
	for _, v := range s.tables {
		t := *v
		t.Players = make([]PlayerView, len(v.Players))
		copy(t.Players, v.Players)
		for i := range t.Players {
			t.Players[i].Hole = nil
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TableID < out[j].TableID })
	return out
}

// FromEngine builds the player rows from engine state and table records.
func FromEngine(st *engine.State, seats []table.Seat, money *table.MoneyTracker) []PlayerView {
	out := make([]PlayerView, 0, len(seats))
	for _, s := range seats {
		p := PlayerView{
			Seat:        s.Index,
			IdentityKey: truncateKey(s.IdentityKey),
			Funded:      s.Funded,
			RefundHeld:  s.RefundHeld,
		}
		if st != nil && s.Index < len(st.Seats) {
			es := st.Seats[s.Index]
			p.Stack = es.Stack
			p.Committed = es.TotalCommit
			p.Folded = es.Folded
			p.AllIn = es.AllIn
		}
		if money != nil {
			if ms, err := money.State(s.Index); err == nil {
				p.MoneySummary = ms.Summary()
				p.AtRisk = ms.AtRisk()
			}
		}
		out = append(out, p)
	}
	return out
}

// BoardStrings renders the community cards.
func BoardStrings(board []cards.Card) []string {
	out := make([]string, 0, len(board))
	for _, c := range board {
		out = append(out, c.String())
	}
	return out
}

func truncateKey(k string) string {
	const n = 16
	if len(k) <= n {
		return k
	}
	return k[:n]
}

// SetLive attaches the playable table the action endpoints drive.
func (s *Store) SetLive(l *LiveTable) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = l
}

// Live returns the playable table, or nil.
func (s *Store) Live() *LiveTable {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.live
}

// Handler serves the UI and its API.
func (s *Store) Handler(identityKey, version, network string) http.Handler {
	mux := http.NewServeMux()

	// The static client.
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// The files are embedded at build time, so this cannot fail at runtime.
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service":     "brc100-poker",
			"version":     version,
			"network":     network,
			"identityKey": identityKey,
			"custody":     "non-custodial: this service holds no player key and cannot move a pot alone",
		})
	})

	mux.HandleFunc("/api/tables", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"tables": s.Tables()})
	})

	mux.HandleFunc("/api/table", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
			return
		}
		// An absent or unparseable seat is an observer, not an error: a spectator link
		// should work without pretending to be a player.
		seat := -1
		if v := r.URL.Query().Get("seat"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &seat); err != nil {
				seat = -1
			}
		}
		view, err := s.Table(id, seat)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, view)
	})

	// ---- playable endpoints ----------------------------------------------
	//
	// Each takes an identity key and resolves it to a seat server-side. A client never states
	// its own seat number, so it cannot act for another player by claiming one.

	mux.HandleFunc("/api/join", func(w http.ResponseWriter, r *http.Request) {
		live := s.Live()
		if live == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no playable table"})
			return
		}
		var req struct {
			IdentityKey string `json:"identityKey"`
			// AgentURL is where this seat's agent serves the substrate. Without it the
			// seat cannot hold its own deal secrets, so the table cannot deal without
			// a dealer.
			AgentURL string `json:"agentUrl"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not valid JSON"})
			return
		}
		seat, err := live.Join(req.IdentityKey)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if req.AgentURL != "" {
			if err := live.RegisterAgent(req.IdentityKey, req.AgentURL); err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"seat": seat, "agentRegistered": req.AgentURL != ""})
	})

	mux.HandleFunc("/api/ready", func(w http.ResponseWriter, r *http.Request) {
		live := s.Live()
		if live == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no playable table"})
			return
		}
		var req struct {
			IdentityKey string `json:"identityKey"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not valid JSON"})
			return
		}
		if err := live.Ready(req.IdentityKey); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/act", func(w http.ResponseWriter, r *http.Request) {
		live := s.Live()
		if live == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no playable table"})
			return
		}
		var req struct {
			IdentityKey string `json:"identityKey"`
			Action      string `json:"action"`
			To          int64  `json:"to"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not valid JSON"})
			return
		}
		if err := live.Act(req.IdentityKey, req.Action, req.To); err != nil {
			// The engine is the adjudicator, and its refusal already says why.
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// /api/live is the single poll a playing client makes: the table as that seat sees it,
	// plus exactly the actions it may take. One call rather than two so the view and the
	// legal actions cannot disagree.
	mux.HandleFunc("/api/live", func(w http.ResponseWriter, r *http.Request) {
		live := s.Live()
		if live == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no playable table"})
			return
		}
		key := r.URL.Query().Get("identityKey")
		seat := live.SeatOf(key)
		writeJSON(w, http.StatusOK, map[string]any{
			"seat":  seat,
			"table": live.View(seat),
			"legal": live.LegalFor(seat),
			// dealerless says whether the deal ran through agents. A player is entitled
			// to know which they got rather than assuming the stronger one.
			"dealerless": live.Dealerless(),
			"winners":    live.Winners(),
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
