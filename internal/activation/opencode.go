package activation

import (
	"errors"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/adapter/opencode"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/ingest"
	"github.com/SupermodularAI/agents-wake/internal/sqlitex"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// openCodeCounters is the per-harness half of a scan's diagnostics for opencode.
//
// Observed is the distinction everything downstream rests on: a store that is not
// there was not looked at, which renders "not observed" and never "0" (ADR-0046),
// while a store that is there and would not open was looked at and collected
// nothing.
type openCodeCounters struct {
	Observed        bool
	Sessions        int
	Parts           int
	Unreadable      int
	ParseErrors     int
	RefusedCalls    int
	PendingCalls    int
	Interrupted     int
	UnknownOutcomes int
	Skipped         int
	OutOfOrderPairs int
	EventsWritten   int
}

// sessionQuery pages opencode's sessions by primary key.
//
// Ordered on the primary key because it is the only indexed order: ORDER BY on an
// unindexed column of a multi-gigabyte store is exactly what ADR-0009 forbids.
// `cost` is not selected — record.Record has no field for it, so reading it would
// be reading something nothing can carry.
const sessionQuery = `SELECT id, coalesce(directory,''), coalesce(version,''),
       coalesce(tokens_input,0), coalesce(tokens_output,0), coalesce(tokens_reasoning,0),
       coalesce(tokens_cache_read,0), coalesce(tokens_cache_write,0), coalesce(time_updated,0)
  FROM session
 WHERE id > ?
 ORDER BY id
 LIMIT ?`

// partQuery pages the tool parts by primary key.
//
// Seven projected columns and no others. The free text opencode keeps in the same
// blob — the tool's input, its output, its title, its error, its raw payload and
// both metadata objects — is never selected, so it does not enter this process at
// all (ADR-0007). Neither is the provider's call id: the canonical identity is the
// row's own primary key, which is unique where that one is not (ADR-0004).
//
// part.id is broadly but not strictly time-ordered — one inversion measured over
// 11,435 rows — which is irrelevant here because the walk reads every row on every
// scan and ADR-0015 makes a cursor an optimisation, never a correctness mechanism.
// The measurement is recorded so nobody later mistakes id for a time ordering.
//
// Neither instant is coalesced, and the start is the one that matters: every other
// coalesce here degrades into a value a validator visibly refuses, while a start
// coalesced to 0 is a plausible 1970 that passes every gate and fabricates a
// duration out of the end instant. Absent has to stay distinguishable from the
// epoch, so it arrives as NULL and the reader refuses the row (plan §3.3, §12).
const partQuery = `SELECT p.id, coalesce(p.session_id,''), coalesce(p.time_updated,0),
       coalesce(json_extract(p.data,'$.tool'),'')         AS tool,
       coalesce(json_extract(p.data,'$.state.status'),'') AS status,
       json_extract(p.data,'$.state.time.start')          AS start_ms,
       json_extract(p.data,'$.state.time.end')            AS end_ms
  FROM part AS p
 WHERE p.id > ?
   AND json_extract(p.data,'$.type') = 'tool'
 ORDER BY p.id
 LIMIT ?`

// ingestOpenCode imports every tool part opencode's store holds that this scope
// admits.
//
// "Could not read" is collecting nothing, never an error that breaks a command
// (plan §4.3): every path below reports counters, and the only error it can return
// is Wake's own store failing to accept a write. The two sentinels sqlitex returns
// are the only distinction drawn — absent means not observed, unreadable means
// observed and blind.
//
// The resolver is the same one Claude Code's walk uses, unchanged. That is what
// makes "consent is per repository, never per harness" true by construction rather
// than by intent: opencode's session.directory goes through the same Identify and
// the same recorded boundary a transcript's cwd does, and this adapter never
// learns either.
func ingestOpenCode(repos *config.Repos, storePath string, servers opencode.Servers,
	destination *store.Store, stale adapter.Staleness, idle adapter.Idleness,
	scope collectionScope, discover *boundaryDiscovery) (int, openCodeCounters, error) {
	counters := openCodeCounters{}

	db, err := sqlitex.Open(storePath)
	switch {
	case errors.Is(err, sqlitex.ErrAbsent):
		// The harness is not installed here. Nothing was looked at, so nothing is
		// reported as zero.
		return 0, counters, nil
	case err != nil:
		return 0, openCodeCounters{Observed: true, Unreadable: 1}, nil
	}
	defer db.Close()
	counters.Observed = true

	// notes stay nil: the skipped-transcript breakdown is keyed by the ordinal the
	// Claude Code walk assigns each source, and this walk has no sources to
	// ordinal. Sharing that buffer would shift Claude Code's ordinals under it.
	walk := ingest.NewOpenCodeScan(resolverFor(repos, scope, discover, nil), servers, stale, idle, destination)

	if pageErr := pageSessions(db, walk, &counters); pageErr != nil {
		counters.Unreadable++
	}
	if pageErr := pageParts(db, walk, &counters); pageErr != nil {
		counters.Unreadable++
	}

	result, closeErr := walk.Close()
	if closeErr != nil {
		return 0, counters, closeErr
	}
	counters.RefusedCalls = result.Refused
	counters.PendingCalls = result.Pending
	counters.Interrupted = result.Interrupted
	counters.UnknownOutcomes = result.UnknownOutcomes
	counters.Skipped = result.SkippedSources
	counters.OutOfOrderPairs = result.OutOfOrderPairs
	counters.EventsWritten = result.Written
	return result.Written, counters, nil
}

// errPageStalled means a full page left the paging key exactly where it was, so
// the next query would return the same rows again. It names no row, no column and
// no path — an error message is exactly where the promise that nothing leaves the
// machine leaks (plan §4.2).
var errPageStalled = errors.New("paging key did not advance")

// page walks one table by primary key until a short page arrives, handing every
// row to visit.
//
// visit returns the key it read — the row's own primary key — whether or not the
// row was usable, and that is what keeps the walk moving: a page whose rows all
// fail to decode still steps past them, so the healthy rows behind it are reached
// and the scan budget is not spent re-reading one page. Advancing only on usable
// rows would re-issue the identical query until MaxRowsPerScan ran out, which on a
// harness that retyped a column is every row in the table, on every future scan.
//
// A full page that leaves the key untouched anyway ends the walk as blindness
// rather than looping: the caller counts it Unreadable, which is the difference
// between "collects nothing" and "collects zero" that doctor must be able to draw
// (plan §3.3, §12).
func page(db *sqlitex.DB, query string, visit func(sqlitex.Row) string) error {
	last := ""
	for {
		started := last
		read, err := db.Page(query, []any{last}, sqlitex.MaxRowsPerPage, func(row sqlitex.Row) error {
			// Rows arrive in key order, so the largest key read is the page's last
			// row; taking the maximum rather than the latest keeps that true even
			// if a row yields no key at all.
			if key := visit(row); key > last {
				last = key
			}
			return nil
		})
		switch {
		case errors.Is(err, sqlitex.ErrRowCap):
			// Partial collection, not a failure: the next scan re-derives what this
			// one did not reach, because every id comes from its source event.
			return nil
		case err != nil:
			return err
		case read < sqlitex.MaxRowsPerPage:
			return nil
		case last == started:
			return errPageStalled
		}
	}
}

// pageSessions registers every session the store holds.
//
// A row that will not scan increments ParseErrors and is skipped, never fatal: one
// malformed row is not a reason to collect nothing from the rest.
func pageSessions(db *sqlitex.DB, walk *ingest.OpenCodeScan, counters *openCodeCounters) error {
	return page(db, sessionQuery, func(row sqlitex.Row) string {
		session, key, usable := scanSession(row, counters)
		if usable {
			counters.Sessions++
			walk.Session(session)
		}
		return key
	})
}

// pageParts offers every tool part the store holds, on the same terms.
func pageParts(db *sqlitex.DB, walk *ingest.OpenCodeScan, counters *openCodeCounters) error {
	return page(db, partQuery, func(row sqlitex.Row) string {
		part, key, usable := scanPart(row, counters)
		if usable {
			counters.Parts++
			walk.Part(part)
		}
		return key
	})
}

// scanSession and scanPart read one row, or count it as a parse error and report
// it unusable. A row this build cannot decode is skipped and never fatal: one
// malformed row is not a reason to collect nothing from the rest, and the count is
// what keeps the skip visible rather than silent (plan §3.3, §12).
//
// Both return the row's primary key beside the value, and return it on the
// refusing path too: row.Scan assigns destinations in order and the key is the
// first of them, so it is read before any later column can refuse, and the pager
// needs it to step past this row. Nothing else survives the refusal — the value
// returned with it is the zero one.
func scanSession(row sqlitex.Row, counters *openCodeCounters) (opencode.Session, string, bool) {
	session := opencode.Session{}
	if err := row.Scan(&session.ID, &session.Directory, &session.Version,
		&session.TokensInput, &session.TokensOutput, &session.TokensReasoning,
		&session.TokensCacheRead, &session.TokensCacheWrite, &session.UpdatedMS); err != nil {
		counters.ParseErrors++
		return opencode.Session{}, session.ID, false
	}
	return session, session.ID, true
}

func scanPart(row sqlitex.Row, counters *openCodeCounters) (opencode.ToolPart, string, bool) {
	part := opencode.ToolPart{}
	// Both instants are legitimately absent and neither is substituted for. A part
	// the harness has not finished has no end, and nil is what makes HasEnd false
	// rather than a duration of zero; a part it recorded no start for has no time
	// at all, and nil is what makes HasStart false rather than the epoch. The
	// reader refuses the second and counts it, which is what keeps the loss visible
	// instead of writing a 1970 record nothing measured.
	var start, end *int64
	if err := row.Scan(&part.ID, &part.SessionID, &part.UpdatedMS,
		&part.Tool, &part.Status, &start, &end); err != nil {
		counters.ParseErrors++
		return opencode.ToolPart{}, part.ID, false
	}
	if start != nil {
		part.StartMS, part.HasStart = *start, true
	}
	if end != nil {
		part.EndMS, part.HasEnd = *end, true
	}
	return part, part.ID, true
}
