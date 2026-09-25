# Sync, freshness and fan-out

How the feed keeps the board consistent with DK, decides what status to show, and fans changes out to browsers.

## Ordering and consistency
- **One writer.** A single apply goroutine owns the Board and calls `Hub.Publish`. Socket frames and
  snapshot results reach it through one channel, in arrival order, so the board and the stream of
  patches can't diverge.
- **Subscribe before snapshot.** On start and on every reconnect: dial, subscribe, wait for the ack,
  and only then request the snapshot. This alone doesn't close the gap: a cached or lagging snapshot
  can be cut before the ack, so an update published in between is in neither. The sync rule below
  covers that.
- **Evidence.** For each entity a frame of the current subscription touched, the apply goroutine keeps
  the values frames set, each field stamped with the receive time of the frame that last set it.
  - A `change` merges its fields into the entry, an `add` replaces the entry with the whole object, and
    a `remove` replaces it with a tombstone. `replacedSelectionId` moves the old ID's fields to the new
    ID and leaves a tombstone on the old one; an `add` that moves a held selection then merges only the
    fields it sent, as the board does, so inherited fields keep their times.
  - Times are per field because `change` is partial: an unrelated later update must neither erase an
    earlier price nor make it look newer than a rejected-frame cutoff.

  A reconnect starts empty:
  a dead subscription's evidence is never used, because a newer update may have been missed after it.
  - Entries don't expire with time; they leave only by the rules below or with their subscription.
    `ponytail:` memory is about one entry per line move per game per subscription; prune entries
    whose event left the board if a subscription ever lives long enough for that to matter.
- **Sync state.** The board is either synced (it passed the checks below, which assume `snapshotLag`
  holds; there is no proof) or needs a resync from a moment `resyncFrom`. These set it:
  - socket loss: needs resync until the next ack;
  - each subscribe ack: `resyncFrom` = the ack;
  - a rejected frame, or a frame part skipped for an unknown ID: `resyncFrom` = its receive time.

  `snapshotLag` is the longest delay between a frame reaching our socket and a snapshot request
  showing it. The capture measurements found a max of 4.2 s, so it is **8 s** (max + 2 s sampling + margin);
  re-check on NFL Sunday. A snapshot's **cut** is `requestStart −
  snapshotLag`: if `snapshotLag` holds, the snapshot reflects every frame received before it.
- **Snapshot commit.** Build a new board from the snapshot → validate → drop superseded evidence →
  content check → overlay evidence → swap. Nothing is dropped unless the board validates and swaps.
  - **Drop superseded evidence.** If there is a `resyncFrom` and the cut is at or after it, drop fields and
    tombstones received before `resyncFrom` (an entry left empty goes). A rejected frame or skipped
    part may have set later values for any of them, which this snapshot holds and no entry does. A
    snapshot cut before `resyncFrom` keeps them. It may already hold the rejected frame's values, but
    nothing shows it does, so keeping the evidence is the conservative choice; the board stays
    unsynced until a snapshot cut at or after `resyncFrom` drops them.
  - **Content check**: every remaining field and tombstone received before the cut matches the snapshot.
    Values are compared as the board reads them, since DK spells some differently in frames and
    snapshots: a missing `main` is true, a missing `isSuspended` is false, `tags` count only for
    `MainPointLine`, and times compare in UTC. `eventScore` is skipped: snapshots never carry it, so
    only the overlay applies it, and after a reconnect a game shows no score until a frame sends one.
    Replaying every captured frame and then committing the later capture gives zero misses in all
    three leagues. A miss has
    two possible causes we can't tell apart: the snapshot is older than `snapshotLag` allows, or it
    holds a newer value whose frame hasn't reached us yet. Either way: count it, stay unsynced, and
    let the next poll retry.
  - **Lost frame.** A field still missing in a snapshot requested ≥ 10 s after its first miss means
    the stream lost a frame (a late frame doesn't stay late that long): force a reconnect. The new
    subscription starts with no evidence, and its post-ack snapshot sets the board.
  - **Overlay**: apply every remaining field and tombstone to the new board. A snapshot never undoes a value the
    current subscription delivered and nothing has superseded. Overlaying a value the snapshot
    already holds is a no-op. An entity the snapshot lacks is rebuilt from its remaining fields plus
    the ones that never change under its ID (a selection's market and outcome, a market's event and
    type, an event's participants). A field whose evidence was dropped stays empty: no price, and a
    market with no suspension evidence shows as suspended.
  - Why overlaying is safe: entries are the latest values of one subscription's ordered stream. The
    only possible regression is an update the snapshot already has and the socket hasn't delivered
    yet; it arrives on the same subscription and overwrites that field. This relies on DK delivering a
    subscription's frames in order and without loss (the captures confirmed the order; loss is caught by the
    lost-frame rule).
  - Overlaying requires frames that set absolute values. The captures confirmed it: `change` is an
    absolute-value field merge, which is why evidence is kept per field.
  - DK has no per-entity versions or timestamps (per the captures), so there is nothing better to compare than
    values.

  The snapshot clears the sync state only if the cut is at or after `resyncFrom` and the content
  check has no miss. An unsynced snapshot still commits if it validates, since after the overlay it
  is the freshest data we have; only the status stays below `live`.
  - DK gives no better evidence. The snapshot has no `Age`, `ETag`, `Last-Modified` or body
    timestamps, and `Date` follows the edge clock (probed 2026-09-22), so its cut can't be read from
    HTTP caching headers.
  - `ponytail:` dropping superseded evidence, and syncing with no evidence to check, rest on the
    measured `snapshotLag` alone. Ceiling: a cache slower than measured, just after a reconnect or
    rejected frame, in a quiet market. Upgrade: per-entity versions, if DK has them.
- **Atomic commit.** A snapshot builds a whole new board and swaps it in. A frame builds new rows for
  the games it touches from copies, validates them, then commits all of them or none. A panic
  (caught by `recover`) or a validation failure rejects the frame: the board is untouched, the
  rejection is logged and counted, and the board needs a resync.


## Freshness
Four separate facts, never mixed:
- **Socket healthy**: subscribed, and a pong or frame within the last 25 s. Transport only.
- **Synced**: see Sync state above. Only a qualifying snapshot sets it; a healthy socket never does.
- **Last sync**: when a snapshot last committed.
- **Last change**: when a price last moved, per game. Display only; it never affects status.

Status is derived from the first three:
- `connecting`: no board yet.
- `live`: socket healthy and synced. A quiet market stays `live` indefinitely.
- `polling`: not `live`, and last sync within 15 s or the scheduler's redial wait running.
- `stale`: neither. `staleSince` is the later of the last `live` moment and the last sync.


## Fan-out
- **Events**: `board` (full, ~10 KB) on connect, then `patch` (changed/removed games) and `status`.
- **Heartbeat**: `status` is re-sent every 15 s. It is a named event with `data`, so it reaches the
  page's listeners; an SSE comment (`: hb`) would not reset the browser watchdog.
- **Handoff**: the Hub holds the current rows and the subscriber set under one mutex.
  - `Publish` updates the rows and enqueues the patch to every subscriber under that mutex.
  - `Subscribe`, under the same mutex, encodes the board from the rows, puts it first in the new
    client's queue, and registers the client.
  - So every client gets its baseline first and every patch after it, with none missed. Sends are
    non-blocking, so the lock never waits on a client.
- **Flushing**: via `http.ResponseController`, with a write deadline.
- **Slow clients**: each client has a 32-message buffer. When it fills, that client is dropped; it
  reconnects and gets a fresh board.
- **Headers**: `text/event-stream`, `Cache-Control: no-cache, no-transform`, `X-Accel-Buffering: no`.

