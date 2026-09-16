'use strict';

// ---------------------------------------------------------------------------
// Every string on this page is untrusted.
//
// A goal, a model's text, a tool's arguments, a tool result, an error message:
// all of it crosses into the browser from somewhere outside the runtime, and
// the eval corpus contains documents that were poisoned on purpose. After the
// MCP work even a *tool description* can come from a server nobody in this
// repo wrote. So the viewer constructs DOM and never parses markup —
// createElement plus textContent, with no exceptions anywhere in this file.
// The markup sinks (innerHTML, insertAdjacentHTML, document.write) and the
// code sinks (eval, new Function) appear nowhere below, and embed_test.go
// fails the build if one reappears.
//
// That test is not a style rule. agentd's entire thesis is that tool output is
// data and never instructions; a viewer that executed the injection the eval
// suite exists to prove the agent resists would be the project refuting itself
// in its own UI. Rejected alternative: escaping strings before an innerHTML
// assignment. It works right up until one call site forgets, and there is no
// test that can tell a forgotten escape from a deliberate one.
// ---------------------------------------------------------------------------

// Event type names, mirroring internal/runtime/events.go.
//
// The list is load-bearing for the stream, not just for rendering: streamRun
// writes "event: <type>" on every frame, so the browser dispatches by name and
// an EventSource with only an onmessage handler would receive nothing at all.
// Each name here gets its own listener. An event type added to the server and
// not to this list is therefore dropped by the browser — which the sequence
// check in accept() then sees as a hole and heals from /events, rendering it
// generically. A silent hole was the outcome worth designing against; a
// generic card is a cosmetic defect.
const EVENT_TYPES = [
  'run_started',
  'model_requested',
  'model_responded',
  'tool_requested',
  'tool_succeeded',
  'tool_failed',
  'budget_exceeded',
  'cancel_requested',
  'run_finished',
];

// Run statuses, in the order a run moves through them (runtime.Statuses).
const STATUSES = ['queued', 'running', 'succeeded', 'failed', 'cancelled', 'budget_exceeded'];

// UUID_RE gates every run id before it is concatenated into a URL or an href.
// The ids come from our own API, so this is belt-and-braces — but an href is a
// script sink (javascript:...) and a path segment is a request forgery, and
// neither costs anything to rule out here.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// How much of a tool result or argument blob is rendered before a "show all"
// button takes over. A single retrieval result can be tens of kilobytes and
// there can be dozens of them in one trajectory; laying all of it out at once
// is what makes a long run's page unusable.
const MAX_INLINE_CHARS = 4000;

// The run list has no change feed, so it polls. The run row does too, for the
// one thing the event log genuinely cannot tell us — see startRowPoll.
const LIST_POLL_MS = 5000;
const ROW_POLL_MS = 5000;

// Manual reconnect backoff, used only when the browser gives up on its own
// retry (see onerror). Capped so a viewer left open against a stopped API
// reconnects within seconds of it coming back rather than minutes.
const REOPEN_MIN_MS = 1000;
const REOPEN_MAX_MS = 15000;

// ---------------------------------------------------------------------------
// DOM helpers
// ---------------------------------------------------------------------------

/** el builds an element. Text always arrives as textContent, never as markup. */
function el(tag, cls, txt) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (txt !== undefined && txt !== null) node.textContent = String(txt);
  return node;
}

/** kv appends a label/value row. */
function kv(parent, key, value) {
  if (value === undefined || value === null || value === '') return;
  const row = el('div', 'kv');
  row.appendChild(el('span', 'kv-k', key));
  row.appendChild(el('span', 'kv-v', value));
  parent.appendChild(row);
}

/** pill renders a status chip, class-qualified so CSS can colour it. */
function pill(status) {
  const known = STATUSES.indexOf(status) >= 0 ? status : 'unknown';
  return el('span', 'pill pill--' + known, status || 'unknown');
}

/** option builds a <select> entry with its value and its label. */
function option(value, label) {
  const opt = el('option', null, label);
  opt.value = value;
  return opt;
}

/** counterCell appends a labelled number to the counter strip. */
function counterCell(parent, label) {
  const cell = el('div', 'counter');
  cell.appendChild(el('div', 'counter-k', label));
  const value = el('div', 'counter-v', '—');
  cell.appendChild(value);
  parent.appendChild(cell);
  return value;
}

/**
 * codeBlock renders a possibly enormous, definitely untrusted string.
 *
 * The truncation is a rendering budget, not a safety measure: textContent is
 * what makes the string safe. CSS does the other half of the job — a poisoned
 * document containing one 40,000-character "word" would otherwise stretch the
 * page rather than wrap inside its box.
 */
function codeBlock(body) {
  const box = el('div', 'code');
  const long = body.length > MAX_INLINE_CHARS;
  const pre = el('pre', null, long ? body.slice(0, MAX_INLINE_CHARS) : body);
  box.appendChild(pre);
  if (long) {
    const more = el('button', 'linkbtn', 'show all (' + num(body.length) + ' chars)');
    more.addEventListener('click', function () {
      pre.textContent = body;
      more.remove();
    });
    box.appendChild(more);
  }
  return box;
}

/** section appends a labelled code block. */
function section(parent, label, body) {
  if (body === undefined || body === null || body === '') return;
  parent.appendChild(el('div', 'sec-label', label));
  parent.appendChild(codeBlock(String(body)));
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

function num(n) {
  const v = Number(n);
  return Number.isFinite(v) ? v.toLocaleString() : '0';
}

/** usd renders micro-dollars, the unit every cost in the event log is in. */
function usd(micro) {
  const v = Number(micro);
  return Number.isFinite(v) ? '$' + (v / 1e6).toFixed(4) : '$0.0000';
}

/** usdMicro parses the decimal strings the runs table stores money as. */
function usdMicro(s) {
  const v = parseFloat(s);
  return Number.isFinite(v) ? Math.round(v * 1e6) : null;
}

function jsonText(value) {
  if (typeof value === 'string') return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch (err) {
    // Cyclic structures cannot come out of JSON.parse, so this is unreachable
    // in practice; it exists so a render never throws away a whole card.
    return String(value);
  }
}

function timeOf(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString();
}

function dateOf(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString();
}

function shortID(id) {
  return typeof id === 'string' && id.length > 8 ? id.slice(0, 8) : String(id || '');
}

/**
 * safeURL rejects anything that is not http(s).
 *
 * The Jaeger base URL is operator-configured and reaches us through
 * GET /v1/runs/:id/trace, so it is not attacker-controlled in any deployment
 * anyone intends — but assigning an unvalidated string to .href is the one
 * remaining way this page could execute someone else's script, and a scheme
 * check is one line.
 */
function safeURL(raw) {
  try {
    const u = new URL(String(raw), location.href);
    return (u.protocol === 'http:' || u.protocol === 'https:') ? u.href : null;
  } catch (err) {
    return null;
  }
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

/**
 * getJSON calls the API and turns a non-2xx into an Error carrying the
 * server's own message, because every handler in api.go answers failures with
 * {"error": "..."} and that text is more useful than a status code alone.
 */
async function getJSON(url, opts) {
  const res = await fetch(url, opts);
  let body = null;
  if ((res.headers.get('content-type') || '').indexOf('application/json') >= 0) {
    try {
      body = await res.json();
    } catch (err) {
      body = null;
    }
  }
  if (!res.ok) {
    const msg = body && typeof body.error === 'string' ? body.error : res.status + ' ' + res.statusText;
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return body;
}

// ---------------------------------------------------------------------------
// Shell and routing
// ---------------------------------------------------------------------------

const viewRoot = document.getElementById('view');
const connPill = document.getElementById('conn');
let teardown = null;

function setConn(state, label) {
  if (!state) {
    connPill.hidden = true;
    return;
  }
  connPill.hidden = false;
  connPill.className = 'conn conn--' + state;
  connPill.textContent = label;
}

function show(node) {
  viewRoot.replaceChildren(node);
}

function errorPanel(message, retry) {
  const panel = el('section', 'panel error');
  panel.appendChild(el('h2', null, 'Something went wrong'));
  panel.appendChild(el('p', null, message));
  if (retry) {
    const btn = el('button', 'btn', 'Retry');
    btn.addEventListener('click', retry);
    panel.appendChild(btn);
  }
  return panel;
}

/**
 * route dispatches on the fragment.
 *
 * Hash routing rather than the History API: the assets are served by a plain
 * http.FileServer over an embed.FS, and real paths would need the server to
 * rewrite every unmatched URL to index.html — which would also swallow typos
 * in the API paths, answering GET /v1/runz with a 200 and a page. A fragment
 * costs one line here and keeps the server a file server.
 */
function route() {
  if (teardown) {
    teardown();
    teardown = null;
  }
  setConn(null);
  const m = /^#\/runs\/([^/?#]+)$/.exec(location.hash);
  if (m) {
    const id = decodeURIComponent(m[1]);
    if (!UUID_RE.test(id)) {
      show(errorPanel('"' + id + '" is not a run id.', function () { location.hash = '#/'; }));
      return;
    }
    teardown = trajectoryView(id);
    return;
  }
  teardown = runListView();
}

window.addEventListener('hashchange', route);
// A backgrounded tab should not keep an SSE connection and two timers alive in
// the bfcache; teardown closes all of them, and a restored page starts over
// from the event log, which is the one place it can safely start over from.
window.addEventListener('pagehide', function () {
  if (teardown) {
    teardown();
    teardown = null;
  }
});
window.addEventListener('pageshow', function (e) {
  if (e.persisted && !teardown) route();
});
route();

// ---------------------------------------------------------------------------
// Run list
// ---------------------------------------------------------------------------

/**
 * runListView renders GET /v1/runs and polls it.
 *
 * It polls rather than streams because there is no change feed for the runs
 * table and inventing one for a page of at most a few dozen rows would be a
 * second SSE endpoint to maintain for a list a human reads at walking pace.
 * A failed load stops the poll: an endpoint that is not deployed yet should
 * produce one error, not one every five seconds forever.
 */
function runListView() {
  const panel = el('section', 'panel');
  const head = el('div', 'panel-head');
  head.appendChild(el('h1', null, 'Runs'));

  const controls = el('div', 'controls');
  const select = el('select', 'input');
  select.appendChild(option('', 'all statuses'));
  for (const s of STATUSES) {
    select.appendChild(option(s, s));
  }
  const refresh = el('button', 'btn', 'Refresh');
  controls.appendChild(select);
  controls.appendChild(refresh);
  head.appendChild(controls);
  panel.appendChild(head);

  const body = el('div', 'panel-body');
  body.appendChild(el('p', 'muted', 'Loading…'));
  panel.appendChild(body);
  show(panel);

  let timer = null;
  let closed = false;

  function stopPoll() {
    if (timer !== null) {
      clearInterval(timer);
      timer = null;
    }
  }

  async function load() {
    const url = '/v1/runs?limit=100' + (select.value ? '&status=' + encodeURIComponent(select.value) : '');
    try {
      const data = await getJSON(url);
      if (closed) return;
      body.replaceChildren(renderRuns((data && data.runs) || []));
    } catch (err) {
      if (closed) return;
      stopPoll();
      body.replaceChildren(errorPanel('Could not load runs: ' + err.message, function () {
        startPoll();
        load();
      }));
    }
  }

  function startPoll() {
    stopPoll();
    timer = setInterval(load, LIST_POLL_MS);
  }

  select.addEventListener('change', load);
  refresh.addEventListener('click', load);
  load();
  startPoll();

  return function () {
    closed = true;
    stopPoll();
  };
}

function renderRuns(runs) {
  if (!runs.length) {
    const empty = el('div', 'empty');
    empty.appendChild(el('p', null, 'No runs yet.'));
    empty.appendChild(el('p', 'muted', 'Submit one with: curl -s localhost:8080/v1/runs -d \'{"goal":"..."}\''));
    return empty;
  }

  const table = el('table', 'runs');
  const thead = el('thead');
  const hrow = el('tr');
  for (const h of ['Status', 'Goal', 'Spend', 'Tokens', 'Started', 'Finished']) {
    hrow.appendChild(el('th', null, h));
  }
  thead.appendChild(hrow);
  table.appendChild(thead);

  const tbody = el('tbody');
  for (const run of runs) {
    const tr = el('tr');

    const tdStatus = el('td');
    tdStatus.appendChild(pill(run.status));
    tr.appendChild(tdStatus);

    const tdGoal = el('td', 'goal-cell');
    const id = String(run.id || '');
    if (UUID_RE.test(id)) {
      // The href is built from an id that matched UUID_RE, so the fragment
      // cannot carry a scheme or anything else that would make this a sink.
      const link = el('a', 'goal-link', run.goal || '(no goal)');
      link.href = '#/runs/' + id;
      link.title = id;
      tdGoal.appendChild(link);
    } else {
      tdGoal.appendChild(el('span', null, run.goal || '(no goal)'));
    }
    tdGoal.appendChild(el('span', 'mono muted id', shortID(id)));
    tr.appendChild(tdGoal);

    tr.appendChild(el('td', 'mono', '$' + (run.spent_usd || '0') + ' / $' + (run.budget_usd || '0')));
    tr.appendChild(el('td', 'mono', num(run.input_tokens) + ' in / ' + num(run.output_tokens) + ' out'));
    tr.appendChild(el('td', 'mono', dateOf(run.created_at)));
    tr.appendChild(el('td', 'mono', run.finished_at ? dateOf(run.finished_at) : '—'));
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  return table;
}

// ---------------------------------------------------------------------------
// Trajectory
// ---------------------------------------------------------------------------

/**
 * trajectoryView renders one run: its history from GET /v1/runs/:id/events,
 * then everything after that from the SSE tail.
 *
 * The counters it shows are folded from the event log here rather than read
 * from the runs row, mirroring internal/runtime/reduce.go. The row is only
 * written as each event is appended, so reading it would mean polling for
 * numbers that are already arriving on an open stream — and the log is the
 * source of truth for exactly this reason.
 */
function trajectoryView(runID) {
  const st = {
    runID: runID,
    // lastSeq is the de-duplication cursor: the highest seq already on screen.
    // Everything about reconnect correctness below is expressed in terms of it.
    lastSeq: 0,
    status: null,
    terminal: false,
    steps: 0,
    maxSteps: null,
    inTok: 0,
    outTok: 0,
    spentMicro: 0,
    budgetMicro: null,
    // Stream bookkeeping, surfaced in the UI because the reconnect contract is
    // the reason this page exists: if it is working, these numbers move and
    // the timeline does not gain or lose a card.
    opens: 0,
    dupes: 0,
    gaps: 0,
    es: null,
    resyncing: false,
    resyncAgain: false,
    reopenTimer: null,
    reopenDelay: REOPEN_MIN_MS,
    rowTimer: null,
    warnSource: null,
    closed: false,
  };

  // --- chrome -------------------------------------------------------------

  const root = el('div', 'traj');

  const back = el('a', 'back', '← all runs');
  back.href = '#/';
  root.appendChild(back);

  const header = el('section', 'panel');
  const titleRow = el('div', 'title-row');
  const statusSlot = el('span');
  statusSlot.appendChild(pill('…'));
  titleRow.appendChild(statusSlot);
  const goalEl = el('h1', 'goal', 'Loading run…');
  titleRow.appendChild(goalEl);
  header.appendChild(titleRow);

  const metaEl = el('div', 'meta');
  header.appendChild(metaEl);

  const counters = el('div', 'counters');
  const cSteps = counterCell(counters, 'steps');
  const cTokens = counterCell(counters, 'tokens');
  const cSpend = counterCell(counters, 'spend');
  const meter = el('div', 'meter');
  const meterFill = el('div', 'meter-fill');
  meter.appendChild(meterFill);
  counters.appendChild(meter);
  header.appendChild(counters);

  const actions = el('div', 'actions');
  const cancelBtn = el('button', 'btn btn--danger', 'Cancel run');
  cancelBtn.disabled = true;
  const traceLink = el('a', 'btn btn--ghost', 'Trace');
  traceLink.hidden = true;
  traceLink.target = '_blank';
  traceLink.rel = 'noopener noreferrer';
  const traceNote = el('span', 'muted');
  const actionNote = el('span', 'muted');
  actions.appendChild(cancelBtn);
  actions.appendChild(traceLink);
  actions.appendChild(traceNote);
  actions.appendChild(actionNote);
  header.appendChild(actions);

  // The stream's own bookkeeping, on screen. A viewer that claims to prove
  // gapless reconnect and shows none of its own evidence is asking to be
  // believed; these three numbers are the evidence.
  const streamLine = el('div', 'stream-line mono muted');
  header.appendChild(streamLine);
  root.appendChild(header);

  const banner = el('div', 'banner');
  banner.hidden = true;
  root.appendChild(banner);

  const timeline = el('section', 'timeline');
  root.appendChild(timeline);

  const waiting = el('p', 'muted waiting', 'No events yet — waiting for a worker to claim this run.');
  timeline.appendChild(waiting);

  show(root);

  function note(text) {
    actionNote.textContent = text;
  }

  // The banner carries one problem at a time, keyed by the fetch that produced
  // it. The run row and the event log fail independently — one Postgres blip
  // can take out either — so a successful retry of one must not silently wipe
  // the other's warning off the screen.
  function warn(source, text) {
    st.warnSource = source;
    banner.hidden = false;
    banner.textContent = text;
  }

  function clearWarn(source) {
    if (st.warnSource !== source) return;
    st.warnSource = null;
    banner.hidden = true;
    banner.textContent = '';
  }

  // --- derived display ----------------------------------------------------

  function setStatus(status) {
    st.status = status;
    statusSlot.replaceChildren(pill(status));
    const done = STATUSES.indexOf(status) >= 2; // succeeded and everything after it
    cancelBtn.disabled = done;
    if (done) cancelBtn.textContent = 'Cancel run';
  }

  function updateCounters() {
    cSteps.textContent = st.maxSteps ? st.steps + ' / ' + st.maxSteps : String(st.steps);
    cTokens.textContent = num(st.inTok) + ' in · ' + num(st.outTok) + ' out';
    cSpend.textContent = st.budgetMicro
      ? usd(st.spentMicro) + ' / ' + usd(st.budgetMicro)
      : usd(st.spentMicro);
    if (st.budgetMicro) {
      const pct = Math.max(0, Math.min(100, (st.spentMicro / st.budgetMicro) * 100));
      // Assigning a computed number through CSSOM, not a style attribute in
      // markup: nothing here is interpolated from a server string.
      meterFill.style.width = pct.toFixed(1) + '%';
      meterFill.className = 'meter-fill' + (pct >= 100 ? ' meter-fill--over' : pct >= 80 ? ' meter-fill--warn' : '');
      meter.hidden = false;
    } else {
      meter.hidden = true;
    }
  }

  function updateStream() {
    const parts = ['seq ' + st.lastSeq];
    if (st.opens > 1) parts.push('reconnects ' + (st.opens - 1));
    if (st.dupes) parts.push('replays dropped ' + st.dupes);
    if (st.gaps) parts.push('gaps healed ' + st.gaps);
    streamLine.textContent = parts.join(' · ');
  }

  function applyRow(run) {
    if (!run) return;
    if (run.goal) goalEl.textContent = run.goal;
    if (!st.terminal) setStatus(run.status);
    if (st.budgetMicro === null) st.budgetMicro = usdMicro(run.budget_usd);
    if (st.maxSteps === null && run.max_steps) st.maxSteps = run.max_steps;

    metaEl.replaceChildren();
    kv(metaEl, 'run', run.id);
    kv(metaEl, 'created', dateOf(run.created_at));
    if (run.finished_at) kv(metaEl, 'finished', dateOf(run.finished_at));
    // The lease is the one fact the event log does not carry: a worker that
    // dies mid-run and a worker that picks the run back up both write no
    // event, so a handover is visible here or nowhere. During the crash demo
    // this cell is what shows the lease gap and then the new owner.
    kv(metaEl, 'lease', run.lease_owner ? run.lease_owner : 'none');
    if (run.lease_expires_at) kv(metaEl, 'lease expires', dateOf(run.lease_expires_at));
    if (run.cancel_requested) kv(metaEl, 'cancel', 'requested');
    updateCounters();
  }

  // --- the fold -----------------------------------------------------------

  /** fold mirrors internal/runtime/reduce.go for the numbers on screen. */
  function fold(ev) {
    const p = ev.payload || {};
    switch (ev.type) {
      case 'run_started':
        if (p.goal) goalEl.textContent = p.goal;
        if (p.max_steps) st.maxSteps = p.max_steps;
        if (p.budget_usd) st.budgetMicro = usdMicro(p.budget_usd);
        if (!st.terminal) setStatus('running');
        break;
      case 'model_responded': {
        st.steps++;
        const u = p.usage || {};
        st.inTok += Number(u.input_tokens || 0);
        st.outTok += Number(u.output_tokens || 0);
        st.spentMicro += Number(p.cost_micro_usd || 0);
        break;
      }
      case 'tool_succeeded':
        // Model spend a tool incurred inside itself moves the run's counters
        // too: spent_usd is the fold of model_responded *and* tool_succeeded
        // costs (ADR-23). Leaving it out here would show a viewer total that
        // disagrees with the budget the worker actually enforces.
        st.spentMicro += Number(p.cost_micro_usd || 0);
        st.inTok += Number(p.input_tokens || 0);
        st.outTok += Number(p.output_tokens || 0);
        break;
      case 'run_finished':
        st.terminal = true;
        setStatus(p.status || 'finished');
        break;
      default:
        break;
    }
  }

  // --- the de-duplication cursor -----------------------------------------

  /**
   * accept decides whether an event is new, and is the whole reconnect
   * contract in six lines.
   *
   * run_events.seq is per-run, starts at 1 and has no holes (Reduce rejects a
   * log where it does), and every delivery path hands events over in seq
   * order. So one integer is a complete cursor:
   *
   *   seq <= lastSeq  a replay. The server was asked to resume from a point
   *                   older than what is on screen — which happens whenever a
   *                   connection drops before delivering anything, because the
   *                   browser then has no Last-Event-ID and the server falls
   *                   back to the query parameter frozen at connect time. The
   *                   card is already rendered; drop it. This is the "no
   *                   repeated step" half of exit criterion 3.
   *
   *   seq == lastSeq+1  the next event. Render it.
   *
   *   seq >  lastSeq+1  a hole: something was delivered and not rendered. It
   *                   should not happen — but "should not happen" is not the
   *                   same as "cannot", and silently rendering past a hole is
   *                   precisely the gap this page was built to disprove. Heal
   *                   from GET /v1/runs/:id/events, which always returns the
   *                   whole log, and count it where a reader can see it.
   */
  function accept(ev) {
    if (!ev || typeof ev.seq !== 'number') return false;
    if (ev.seq <= st.lastSeq) {
      st.dupes++;
      updateStream();
      return false;
    }
    if (ev.seq > st.lastSeq + 1) {
      st.gaps++;
      updateStream();
      resync();
      return false;
    }
    return true;
  }

  function render(ev) {
    if (!accept(ev)) return;
    // Stick to the bottom only if the reader is already there, measured
    // before the append changes the page height.
    const stick = (window.innerHeight + window.scrollY) >= (document.body.scrollHeight - 200);
    fold(ev);
    waiting.hidden = true;
    timeline.appendChild(eventCard(ev));
    st.lastSeq = ev.seq;
    updateCounters();
    updateStream();
    if (ev.type === 'run_finished') finish();
    if (stick) window.scrollTo(0, document.body.scrollHeight);
  }

  /**
   * resync renders everything above lastSeq from the full event log.
   *
   * It is both the initial history load (lastSeq is 0, so it renders
   * everything) and the repair path for a hole. The do/while closes the window
   * where an event arrives on the stream *during* the fetch: accept() drops it
   * and sets resyncAgain, and the loop goes round once more rather than
   * leaving a hole that nothing would ever come back for.
   */
  async function resync() {
    if (st.resyncing) {
      st.resyncAgain = true;
      return;
    }
    st.resyncing = true;
    try {
      do {
        st.resyncAgain = false;
        const data = await getJSON('/v1/runs/' + st.runID + '/events');
        if (st.closed) return;
        for (const ev of (data && data.events) || []) {
          if (ev.seq > st.lastSeq) render(ev);
        }
      } while (st.resyncAgain);
      clearWarn('log');
    } catch (err) {
      if (st.closed) return;
      warn('log', 'Could not load the event log: ' + err.message);
    } finally {
      st.resyncing = false;
    }
  }

  // --- the stream ---------------------------------------------------------

  /**
   * openStream tails the log, resuming from lastSeq.
   *
   * Two different resume points travel to streamRun, and which one wins is the
   * point of the design:
   *
   *  - On the FIRST connection the browser has no Last-Event-ID to send, so the
   *    cursor goes in the query parameter lastEventID() falls back to. Without
   *    it the server would replay from seq 1 and every event the history fetch
   *    just rendered would arrive again — correct on screen, thanks to
   *    accept(), but a whole log re-sent for nothing.
   *
   *  - On a RECONNECT the browser re-requests this same URL — stale query
   *    parameter and all — and adds a Last-Event-ID header holding the id: of
   *    the last frame it delivered. lastEventID() reads the header first, so
   *    the newer cursor wins and the replay starts exactly where the
   *    trajectory stops. The stale parameter is consulted only when the header
   *    is absent, which is the one case where it is the better answer: a
   *    connection that died before delivering a single event, where the cursor
   *    frozen at connect time is still current.
   *
   * Either way the client-side cursor is authoritative, because accept() drops
   * anything at or below it. Pull the network cable mid-run and the trajectory
   * continues with no gap and no repeat; that is exit criterion 3.
   */
  function openStream() {
    closeStream();
    const url = '/v1/runs/' + st.runID + '/stream?last_event_id=' + st.lastSeq;
    const es = new EventSource(url);
    st.es = es;
    setConn('connecting', 'connecting…');

    es.addEventListener('open', function () {
      st.opens++;
      st.reopenDelay = REOPEN_MIN_MS;
      setConn('live', st.opens > 1 ? 'live · resumed at seq ' + st.lastSeq : 'live');
      updateStream();
    });

    for (const type of EVENT_TYPES) {
      es.addEventListener(type, onFrame);
    }
    // streamRun names every event, so this never fires today. It is here so
    // that an unnamed frame — a future keepalive carrying data, a proxy that
    // rewrites the event field — is rendered rather than silently dropped.
    es.addEventListener('message', onFrame);

    es.addEventListener('error', function () {
      if (st.closed || st.terminal) return;
      if (es.readyState === EventSource.CONNECTING) {
        // The browser is retrying on its own and will send Last-Event-ID when
        // it does. Nothing to do but say so.
        setConn('retry', 'reconnecting…');
        return;
      }
      // CLOSED: the browser has given up for good, which is what it does when
      // the response is not a 2xx text/event-stream — an API restart caught
      // mid-handshake, or a proxy answering 502. Its automatic retry is not
      // coming, so reopen on our own schedule from the cursor we hold.
      scheduleReopen();
    });
  }

  function onFrame(msg) {
    if (st.closed) return;
    let ev = null;
    try {
      ev = JSON.parse(msg.data);
    } catch (err) {
      warn('frame', 'Dropped a malformed event frame.');
      return;
    }
    render(ev);
  }

  function scheduleReopen() {
    closeStream();
    if (st.reopenTimer !== null) return;
    const delay = st.reopenDelay;
    st.reopenDelay = Math.min(REOPEN_MAX_MS, st.reopenDelay * 2);
    setConn('retry', 'reconnecting in ' + Math.round(delay / 1000) + 's (from seq ' + st.lastSeq + ')');
    st.reopenTimer = setTimeout(function () {
      st.reopenTimer = null;
      if (st.closed || st.terminal) return;
      // Re-fetching the log first would also be correct, but the stream's own
      // replay is the contract under test; letting it do the work is what
      // makes a dropped connection prove something.
      openStream();
    }, delay);
  }

  function closeStream() {
    if (st.es) {
      st.es.close();
      st.es = null;
    }
  }

  /**
   * finish closes the stream on the terminal event.
   *
   * This is not tidying: EventSource reconnects after *any* close, including
   * the clean one streamRun performs after run_finished. Leaving it open would
   * reconnect to a stream that can never emit another event and hold it there
   * on keepalives forever, once per finished run anyone happened to look at.
   */
  function finish() {
    st.terminal = true;
    closeStream();
    stopRowPoll();
    cancelBtn.disabled = true;
    setConn('done', 'stream closed · run finished');
    // One last read of the row, for finished_at and the released lease.
    loadRow();
  }

  // --- the run row --------------------------------------------------------

  /**
   * loadRow reads GET /v1/runs/:id for the parts of a run that are not in the
   * event log: the budget, the timestamps, and the lease.
   *
   * A failure here is a banner rather than a dead view, because the event log
   * is the source of truth and the timeline below can render perfectly well
   * without the row. A 404 is different: there is no run to render at all.
   */
  async function loadRow() {
    try {
      const data = await getJSON('/v1/runs/' + st.runID);
      if (st.closed) return;
      applyRow(data && data.run);
      clearWarn('row');
    } catch (err) {
      if (st.closed) return;
      if (err.status === 404) {
        show(errorPanel('Run ' + st.runID + ' does not exist.', function () { location.hash = '#/'; }));
        st.closed = true;
        return;
      }
      warn('row', 'Could not load the run: ' + err.message);
    }
  }

  function startRowPoll() {
    stopRowPoll();
    st.rowTimer = setInterval(loadRow, ROW_POLL_MS);
  }

  function stopRowPoll() {
    if (st.rowTimer !== null) {
      clearInterval(st.rowTimer);
      st.rowTimer = null;
    }
  }

  // --- actions ------------------------------------------------------------

  cancelBtn.addEventListener('click', async function () {
    cancelBtn.disabled = true;
    note('cancelling…');
    try {
      await getJSON('/v1/runs/' + st.runID + '/cancel', { method: 'POST' });
      // The flag is cooperative: the worker finishes the run at its next loop
      // iteration, and cancel_requested then run_finished arrive on the
      // stream. Nothing to re-render here.
      note('cancel requested');
    } catch (err) {
      note(err.status === 409 ? 'run already finished' : 'cancel failed: ' + err.message);
      cancelBtn.disabled = st.terminal;
    }
  });

  async function loadTrace() {
    try {
      const t = await getJSON('/v1/runs/' + st.runID + '/trace');
      if (st.closed) return;
      const href = safeURL(t && t.url);
      if (!href) {
        traceNote.textContent = 'trace link rejected (not http)';
        return;
      }
      traceLink.href = href;
      traceLink.textContent = 'Trace ' + shortID(t.trace_id);
      traceLink.hidden = false;
    } catch (err) {
      if (st.closed) return;
      // A 404 here is the documented answer for a run submitted with tracing
      // off, not a failure worth a banner.
      traceNote.textContent = err.status === 404 ? 'no trace recorded' : 'trace unavailable: ' + err.message;
    }
  }

  // --- start --------------------------------------------------------------

  (async function start() {
    await loadRow();
    if (st.closed) return;
    loadTrace();
    // History first, stream second, and in that order: opening the stream
    // first would need every frame buffered until the fetch landed, to keep
    // them behind the history they overlap. Doing it this way, anything
    // written between the two lands in the stream's replay because the cursor
    // sent with it is the last seq the fetch returned.
    await resync();
    if (st.closed) return;
    if (st.terminal) {
      // A finished run has nothing to tail. Opening the stream would connect,
      // replay nothing, and sit on keepalives.
      setConn('done', 'run finished · nothing to stream');
      return;
    }
    openStream();
    startRowPoll();
  })();

  return function () {
    st.closed = true;
    closeStream();
    stopRowPoll();
    if (st.reopenTimer !== null) {
      clearTimeout(st.reopenTimer);
      st.reopenTimer = null;
    }
  };
}

// ---------------------------------------------------------------------------
// Event cards
// ---------------------------------------------------------------------------

const TYPE_LABEL = {
  run_started: 'run started',
  model_requested: 'model requested',
  model_responded: 'model responded',
  tool_requested: 'tool requested',
  tool_succeeded: 'tool succeeded',
  tool_failed: 'tool failed',
  budget_exceeded: 'budget exceeded',
  cancel_requested: 'cancel requested',
  run_finished: 'run finished',
};

function eventCard(ev) {
  const card = el('article', 'ev ev--' + (EVENT_TYPES.indexOf(ev.type) >= 0 ? ev.type : 'unknown'));

  const head = el('header', 'ev-head');
  head.appendChild(el('span', 'seq mono', '#' + ev.seq));
  head.appendChild(el('span', 'ev-type', TYPE_LABEL[ev.type] || ev.type));
  head.appendChild(el('span', 'ev-time mono muted', timeOf(ev.created_at)));
  card.appendChild(head);

  const body = el('div', 'ev-body');
  const render = CARDS[ev.type];
  const payload = ev.payload || {};
  if (render) {
    render(payload, body);
  } else {
    // An event type this viewer predates. Showing the payload verbatim beats
    // showing nothing, and is why the stream's gap check heals rather than
    // just complains.
    section(body, 'payload', jsonText(payload));
  }
  card.appendChild(body);

  if (render) card.appendChild(rawDetails(payload));
  return card;
}

/**
 * rawDetails hangs the full payload off a disclosure that serialises it on
 * first open. A trajectory can hold dozens of multi-kilobyte tool results, and
 * stringifying all of them up front to fill boxes nobody opens is the
 * difference between a page that renders instantly and one that stutters.
 */
function rawDetails(payload) {
  const d = el('details', 'raw');
  d.appendChild(el('summary', null, 'payload'));
  let built = false;
  d.addEventListener('toggle', function () {
    if (built || !d.open) return;
    built = true;
    d.appendChild(codeBlock(jsonText(payload)));
  });
  return d;
}

const CARDS = {
  run_started: function (p, body) {
    const cfg = p.agent_config || {};
    kv(body, 'worker', p.worker);
    kv(body, 'model', cfg.model || '(default)');
    kv(body, 'max steps', p.max_steps);
    kv(body, 'budget', p.budget_usd ? '$' + p.budget_usd : null);
    kv(body, 'tools', (cfg.tools || []).join(', ') || '(none)');
    if (cfg.tool_delay_ms) kv(body, 'tool delay', cfg.tool_delay_ms + ' ms');
    section(body, 'goal', p.goal);
    if (cfg.system_prompt) section(body, 'system prompt', cfg.system_prompt);
  },

  model_requested: function (p, body) {
    const params = p.params || {};
    kv(body, 'step', p.step);
    kv(body, 'model', p.model);
    kv(body, 'max tokens', params.max_tokens);
    kv(body, 'tools offered', (params.tools || []).length);
    // The prompt hash is what cassette matching keys on; showing a prefix is
    // enough to spot two identical prompts in a row, which is what a stuck
    // loop looks like.
    kv(body, 'messages sha256', shortID(p.messages_sha256));
  },

  model_responded: function (p, body) {
    const u = p.usage || {};
    kv(body, 'step', p.step);
    kv(body, 'model', (p.provider ? p.provider + ' · ' : '') + (p.model || ''));
    kv(body, 'stop reason', p.stop_reason);
    kv(body, 'tokens', num(u.input_tokens) + ' in · ' + num(u.output_tokens) + ' out' +
      (u.cache_read_input_tokens ? ' · ' + num(u.cache_read_input_tokens) + ' cached' : ''));
    kv(body, 'cost', usd(p.cost_micro_usd));
    for (const block of p.content || []) {
      body.appendChild(contentBlock(block));
    }
  },

  tool_requested: function (p, body) {
    kv(body, 'tool', p.name);
    kv(body, 'tool_use_id', p.tool_use_id);
    section(body, 'arguments', jsonText(p.args));
  },

  tool_succeeded: function (p, body) {
    kv(body, 'tool', p.name);
    kv(body, 'duration', p.duration_ms + ' ms');
    kv(body, 'exit code', p.exit_code);
    if (p.replayed) {
      // The crash demo's payoff: a completed tool call read back from the
      // ledger by the worker that took the run over, rather than run twice.
      const badge = el('span', 'badge badge--replayed', 'replayed from ledger');
      body.appendChild(badge);
    }
    if (p.cost_micro_usd) {
      kv(body, 'model spend inside tool', usd(p.cost_micro_usd) + (p.cost_model ? ' · ' + p.cost_model : ''));
    }
    section(body, 'result', typeof p.result === 'string' ? p.result : jsonText(p.result));
  },

  tool_failed: function (p, body) {
    kv(body, 'tool', p.name);
    kv(body, 'tool_use_id', p.tool_use_id);
    kv(body, 'retryable', p.retryable ? 'yes' : 'no');
    section(body, 'error', p.error);
  },

  budget_exceeded: function (p, body) {
    kv(body, 'spent', usd(p.spent_micro_usd));
    kv(body, 'budget', usd(p.budget_micro_usd));
    // "would_exceed" is the pre-flight refusal and "spent" the post-hoc
    // backstop (ADR-22); which one fired is the first question anyone asks of
    // a run that stopped on budget.
    kv(body, 'reason', p.reason);
    if (p.estimate_micro_usd) {
      kv(body, 'refused call estimate', usd(p.estimate_micro_usd));
      kv(body, 'estimated input tokens', num(p.estimated_input_tokens));
      kv(body, 'max output tokens', num(p.max_output_tokens));
    }
  },

  cancel_requested: function (p, body) {
    kv(body, 'source', p.source);
    kv(body, 'phase', p.phase);
  },

  run_finished: function (p, body) {
    kv(body, 'status', p.status);
    section(body, 'final answer', p.final_answer);
    section(body, 'error', p.error);
  },
};

/** contentBlock renders one block of an assistant turn. */
function contentBlock(block) {
  const wrap = el('div', 'block block--' + (block.type || 'unknown'));
  switch (block.type) {
    case 'text':
      wrap.appendChild(el('div', 'sec-label', 'text'));
      wrap.appendChild(codeBlock(block.text || ''));
      break;
    case 'tool_use':
      wrap.appendChild(el('div', 'sec-label', 'tool_use → ' + (block.name || '?')));
      wrap.appendChild(codeBlock(jsonText(block.input)));
      break;
    case 'thinking': {
      // Collapsed by default: it is long, it is rarely what someone opened the
      // page for, and it is model output like everything else here.
      const d = el('details', 'raw');
      d.appendChild(el('summary', null, 'thinking'));
      d.appendChild(codeBlock(block.thinking || '(omitted)'));
      wrap.appendChild(d);
      break;
    }
    case 'redacted_thinking':
      wrap.appendChild(el('div', 'sec-label', 'redacted thinking'));
      wrap.appendChild(el('p', 'muted', 'encrypted by the provider; passed through untouched'));
      break;
    default:
      wrap.appendChild(el('div', 'sec-label', block.type || 'block'));
      wrap.appendChild(codeBlock(jsonText(block)));
  }
  return wrap;
}
