// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import {
  INTENTS_VIEW_FILES,
  OUTPUT_KEEP,
  OUTPUT_LIMIT_NOTE,
  costShare,
  formatDuration,
  groupByColumn,
  mergeRunOutput,
  outputSegments,
  outputStatus,
  secondsSince,
  stopConditions,
} from "../src/intents.ts";
import { hrefForIntent, hrefForIntentRun, isIntentsRoute, parseRoute } from "../src/router.ts";

const card = (name, column, phaseSince) => ({
  name,
  project: "p",
  issue: 1,
  phase: "Building",
  column,
  revisions: 0,
  checkFixes: 0,
  costMicroUSD: 0,
  phaseSince,
});

test("groupByColumn sorts cards into columns, newest move first", () => {
  const cols = groupByColumn([
    card("a", "building", "2026-10-06T10:00:00Z"),
    card("b", "building", "2026-10-06T11:00:00Z"),
    card("c", "done", "2026-10-06T09:00:00Z"),
    card("d", "mystery", "2026-10-06T09:00:00Z"),
  ]);
  assert.deepEqual(
    cols.building.map((c) => c.name),
    ["b", "a"],
  );
  assert.deepEqual(
    cols.done.map((c) => c.name),
    ["c"],
  );
  // A column the board does not know lands in "blocked", never nowhere.
  assert.deepEqual(
    cols.blocked.map((c) => c.name),
    ["d"],
  );
  assert.deepEqual(cols.planning, []);
});

test("formatDuration", () => {
  assert.equal(formatDuration(45), "45s");
  assert.equal(formatDuration(250), "4m 10s");
  assert.equal(formatDuration(3900), "1h 05m");
  assert.equal(formatDuration(3 * 86400 + 7200), "3d 2h");
  assert.equal(formatDuration(-1), "—");
  assert.equal(formatDuration(undefined), "—");
  assert.equal(formatDuration(Number.NaN), "—");
});

test("secondsSince never goes negative and ignores garbage", () => {
  const now = Date.parse("2026-10-06T12:00:00Z");
  assert.equal(secondsSince("2026-10-06T11:59:00Z", now), 60);
  assert.equal(secondsSince("2026-10-06T12:05:00Z", now), 0);
  assert.equal(secondsSince("not a time", now), undefined);
  assert.equal(secondsSince(undefined, now), undefined);
});

test("costShare clamps and flags the ceiling", () => {
  assert.deepEqual(costShare(5_000_000, 10_000_000), { share: 0.5, over: false });
  assert.deepEqual(costShare(12_000_000, 10_000_000), { share: 1, over: true });
  assert.deepEqual(costShare(1, 0), { share: 0, over: false });
});

const now = Date.parse("2026-10-06T12:00:00Z");
const run = {
  name: "demo-1-bld-r1-app-a2",
  intent: "demo-1",
  project: "demo",
  tier: "intents",
  stage: "build",
  round: 1,
  attempt: 2,
  countedAttempt: 2,
  running: true,
  startedAt: "2026-10-06T11:37:00Z",
  grant: { maxTurns: 150, tokenBudget: 800000, timeoutMilliseconds: 3_600_000 },
  job: { startedAt: "2026-10-06T11:36:00Z", deadlineSeconds: 5400, idleTimeoutSeconds: 1200 },
  lastAttempt: true,
  limits: { maxRevisions: 3, maxCheckFixes: 2, maxCostMicroUSD: 10_000_000, maxAttempts: 2 },
  intentCostMicroUSD: 2_500_000,
};

test("stopConditions says what ends a running run, and when", () => {
  const lines = stopConditions(run, { turns: 61, lastAt: "2026-10-06T11:50:00Z", live: true }, now);
  assert.deepEqual(lines, [
    "stops at the 1h 00m stage time limit in 37m 00s",
    "the Job's 1h 30m deadline ends it in 1h 06m",
    "ends after 20m 00s with no model turn or tool result (last activity 10m 00s ago)",
    "turn limit 150 (61 transcript entries so far; not every entry is a turn)",
    "output-token kill switch at 800,000 streamed tokens (not a spend bound)",
    "last attempt: if it fails, the intent fails",
    "cost ceiling $10.00 is checked only before the next launch (recorded so far $2.50); this run can pass it",
  ]);
});

test("stopConditions never reads a frozen time past the transcript cap as idleness", () => {
  // The recorder stopped 25 minutes ago at its cap; the agent kept going.
  const activity = { turns: 501, lastAt: "2026-10-06T11:35:00Z", live: true, capped: true };
  const lines = stopConditions(run, activity, now);
  const idle = lines.find((l) => l.startsWith("ends after 20m 00s"));
  assert.ok(idle, lines.join("\n"));
  assert.ok(!idle.includes("last activity"), idle);
  assert.ok(idle.includes("transcript cap"), idle);
  const turns = lines.find((l) => l.startsWith("turn limit"));
  assert.ok(!turns.includes("so far"), turns);
});

test("stopConditions for a revise round's earlier attempt", () => {
  const lines = stopConditions(
    { ...run, stage: "revise", lastAttempt: false, attempt: 1, countedAttempt: 1, job: undefined },
    null,
    now,
  );
  assert.ok(lines.includes("attempt 1 of 2: a failure is retried"));
  assert.ok(!lines.some((l) => l.includes("deadline")));
  const last = stopConditions({ ...run, stage: "revise" }, null, now);
  assert.ok(last.includes("last attempt: if it fails, the round ends"));
});

test("stopConditions counts attempts as the controller does, not by ordinal", () => {
  // Attempts 1 and 2 never ran their agent (no node could fit it), so
  // ordinal 3 is the first attempt that counts.
  const lines = stopConditions({ ...run, attempt: 3, countedAttempt: 1, lastAttempt: false }, null, now);
  assert.ok(lines.includes("attempt 1 of 2: a failure is retried"), lines.join("\n"));
  assert.ok(!lines.some((l) => l.includes("attempt 3")));
});

test("intents routes parse and round-trip", () => {
  assert.deepEqual(parseRoute("#/intents"), { view: "intents" });
  assert.deepEqual(parseRoute(hrefForIntent("demo-7")), { view: "intent", name: "demo-7" });
  assert.deepEqual(parseRoute(hrefForIntentRun("demo-7", "demo-7-plan-r1-a1")), {
    view: "intentRun",
    name: "demo-7",
    run: "demo-7-plan-r1-a1",
  });
  assert.equal(isIntentsRoute(parseRoute("#/intents/demo-7")), true);
  assert.equal(isIntentsRoute(parseRoute("#/finding/x")), false);
});

const chunk = (line, lines, extra = {}) => ({ task: "b1", line, lines, ...extra });
const fold = (chunks, state = null) => chunks.reduce(mergeRunOutput, state);
const numbers = (state) => state.lines.map((l) => l.n);

test("mergeRunOutput holds lines by number and drops a reconnect's replay of them", () => {
  const live = fold([chunk(1, ["a", "b"]), chunk(3, ["c"])]);
  // A reconnect replays the server's ring: lines already held are not
  // added twice, and the text first held is kept.
  const after = fold([chunk(1, ["A", "B", "C"]), chunk(4, ["d"])], live);
  assert.deepEqual(
    after.lines.map((l) => l.text),
    ["a", "b", "c", "d"],
  );
  assert.equal(after.end, 5);
  assert.deepEqual(outputSegments(after), [{ kind: "lines", text: "a\nb\nc\nd" }]);
});

test("mergeRunOutput records gaps, and a replay can fill one", () => {
  // Lines 3-9 never arrived (a chunk dropped on a slow connection), and
  // lines past 12 are known to exist from the last chunk's number.
  const gapped = fold([chunk(1, ["a", "b"]), chunk(10, ["j", "k"]), chunk(13, [])]);
  assert.deepEqual(outputSegments(gapped), [
    { kind: "lines", text: "a\nb" },
    { kind: "skipped", count: 7 },
    { kind: "lines", text: "j\nk" },
    { kind: "skipped", count: 1 },
  ]);
  // A late joiner whose replay starts at line 5 is told about lines 1-4.
  assert.deepEqual(outputSegments(fold([chunk(5, ["e"])]))[0], { kind: "skipped", count: 4 });
  // The reconnect replay brings lines 3-9 back: the gap closes.
  const filled = fold([chunk(3, ["c", "d", "e", "f", "g", "h", "i"])], gapped);
  assert.deepEqual(numbers(filled), [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11]);
  assert.deepEqual(outputSegments(filled).at(-1), { kind: "skipped", count: 1 });
});

test("mergeRunOutput starts over for another command", () => {
  const first = fold([chunk(1, ["old"], { done: true, truncated: true })]);
  const next = mergeRunOutput(first, { task: "b2", line: 1, lines: ["new"] });
  assert.deepEqual(next, { task: "b2", lines: [{ n: 1, text: "new" }], end: 2, done: false, truncated: false });
});

test("mergeRunOutput keeps only the newest OUTPUT_KEEP lines", () => {
  const many = Array.from({ length: OUTPUT_KEEP + 120 }, (_, i) => `l${i + 1}`);
  const state = fold([chunk(1, many.slice(0, 300)), chunk(301, many.slice(300))]);
  assert.equal(state.lines.length, OUTPUT_KEEP);
  assert.equal(state.lines[0].n, 121);
  assert.equal(state.lines.at(-1).text, `l${OUTPUT_KEEP + 120}`);
  // What was let go is shown as lines not shown, and a replay of it does
  // not push the newest lines out.
  assert.deepEqual(outputSegments(state)[0], { kind: "skipped", count: 120 });
  assert.deepEqual(numbers(fold([chunk(1, many.slice(0, 50))], state)), numbers(state));
});

test("mergeRunOutput tracks done and truncated, and outputStatus names them", () => {
  const running = fold([chunk(1, ["a"])]);
  assert.equal(outputStatus(running, true), "running");
  assert.equal(outputStatus(running, false), "no longer followed");
  const done = mergeRunOutput(running, chunk(2, [], { done: true }));
  assert.equal(done.done, true);
  assert.equal(outputStatus(done, true), "finished");
  assert.equal(outputStatus(done, false), "finished");
  // A later chunk does not undo either flag.
  const cut = fold([chunk(2, [], { truncated: true }), chunk(3, [])], running);
  assert.equal(cut.truncated, true);
  assert.equal(cut.done, false);
  assert.equal(cut.end, 3);
});

// The header ranks the command's state: its end outranks everything, a
// stream that ended without it knows nothing more, and a limit the agent
// hit is said beside "running" only, never in its place, since the command
// goes on. A gap in the line numbers (a sample) is not a limit.
test("outputStatus ranks finished, no longer followed, the limit, then running", () => {
  const state = (extra) => ({ task: "b1", lines: [], end: 1, done: false, truncated: false, ...extra });
  const cases = [
    [{ done: true }, true, "finished"],
    [{ done: true }, false, "finished"],
    [{ done: true, truncated: true }, true, "finished · live output limit reached"],
    [{ done: true, truncated: true }, false, "finished · live output limit reached"],
    [{}, false, "no longer followed"],
    [{ truncated: true }, false, "no longer followed"],
    [{ truncated: true }, true, "running · live output limit reached"],
    [{}, true, "running"],
  ];
  for (const [extra, following, want] of cases) {
    assert.equal(outputStatus(state(extra), following), want, JSON.stringify({ extra, following }));
  }
  // Samples leave gaps and are not truncated: still just running.
  const sampled = fold([chunk(1, ["a"]), chunk(50, ["x"]), chunk(90, ["y"])]);
  assert.equal(outputStatus(sampled, true), "running");
});

// The agent's process budget running out on a running command: one chunk
// marks its live output truncated past the lines it dropped, not done, and a
// second, once the command ends, marks it done at the same number.
test("outputStatus follows a budget cut from running to finished", () => {
  const cut = fold([chunk(1, ["a", "b"]), chunk(9, [], { truncated: true })]);
  assert.equal(outputStatus(cut, true), `running · ${OUTPUT_LIMIT_NOTE}`);
  const ended = mergeRunOutput(cut, chunk(9, [], { done: true, truncated: true }));
  assert.equal(outputStatus(ended, true), `finished · ${OUTPUT_LIMIT_NOTE}`);
  assert.equal(ended.end, 9);
  assert.deepEqual(outputSegments(ended).at(-1), { kind: "skipped", count: 6 });
});

test("mergeRunOutput ignores a malformed chunk", () => {
  const state = fold([chunk(1, ["a"])]);
  for (const bad of [
    { task: "", line: 1, lines: ["x"] },
    { task: "b1", line: 0, lines: ["x"] },
    { task: "b1", line: 1.5, lines: ["x"] },
    { line: 1, lines: ["x"] },
    null,
  ]) {
    assert.equal(mergeRunOutput(state, bad), state, JSON.stringify(bad));
  }
  assert.equal(mergeRunOutput(null, { task: "b1", line: -1, lines: [] }), null);
  // A chunk without lines is still a chunk: it moves the end.
  assert.equal(mergeRunOutput(state, { task: "b1", line: 4 }).end, 4);
});

// mulberry32 is a tiny seeded PRNG, so the property below runs the same
// cases every time without a dependency.
function mulberry32(seed) {
  return () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// randomStream is one command's chunks as a follow might deliver them:
// mostly consecutive, with gaps (samples, drops), overlaps and repeats
// (replays), and empty chunks; a line's text is its number's.
function randomStream(rand) {
  const int = (n) => Math.floor(rand() * n);
  const out = [];
  let cur = 1;
  for (let i = 1 + int(60); i > 0; i--) {
    const roll = rand();
    const line = Math.max(1, roll < 0.6 ? cur : roll < 0.8 ? cur + 1 + int(80) : cur - int(30));
    const lines = Array.from({ length: int(40) }, (_, k) => `L${line + k}`);
    out.push(chunk(line, lines, { done: i === 1 && rand() < 0.5, truncated: rand() < 0.05 }));
    cur = Math.max(cur, line + lines.length);
  }
  return out;
}

// Whatever a command's chunks are, and wherever a reconnect cuts in, the
// panel's state is what one pass over the stream gives: replaying any
// prefix again (the replay a reconnect brings) changes nothing. It keeps at
// most OUTPUT_KEEP lines, each under its own number, and its segments
// account for every line up to the end exactly once, held or counted as
// not shown.
test("mergeRunOutput and outputSegments hold under replays (seeded property)", () => {
  const rand = mulberry32(20261006);
  for (let run = 0; run < 300; run++) {
    const stream = randomStream(rand);
    const once = fold(stream);
    const cut = Math.floor(rand() * (stream.length + 1));
    const again = fold([...stream.slice(0, cut), ...stream.slice(0, cut), ...stream.slice(cut)]);
    assert.deepEqual(again, once, `run ${run}: a replay of the first ${cut} chunks changed the state`);

    assert.ok(once.lines.length <= OUTPUT_KEEP, `run ${run}: ${once.lines.length} lines held`);
    once.lines.forEach((l, i) => {
      assert.equal(l.text, `L${l.n}`, `run ${run}: line ${l.n} holds ${l.text}`);
      if (i > 0) assert.ok(l.n > once.lines[i - 1].n, `run ${run}: lines out of order`);
    });

    const segments = outputSegments(once);
    const shown = segments.filter((g) => g.kind === "lines").reduce((n, g) => n + g.text.split("\n").length, 0);
    const skipped = segments.filter((g) => g.kind === "skipped").reduce((n, g) => n + g.count, 0);
    assert.equal(shown, once.lines.length, `run ${run}: segments show ${shown} of ${once.lines.length} lines`);
    assert.equal(shown + skipped, once.end - 1, `run ${run}: segments cover ${shown + skipped} of ${once.end - 1}`);
    assert.ok(
      segments.every((g) => g.kind === "lines" || g.count > 0),
      `run ${run}: an empty gap`,
    );
  }
});

// Agent text is rendered as plain text only: no intents view may reach the
// markdown renderer, or put raw HTML into the page.
test("no intents view imports Markdown or sets inner HTML", () => {
  for (const file of INTENTS_VIEW_FILES) {
    const src = readFileSync(new URL(`../${file}`, import.meta.url), "utf8");
    assert.doesNotMatch(src, /from\s+["']\.\/Markdown["']/, `${file} imports Markdown`);
    assert.doesNotMatch(src, /dangerouslySetInnerHTML|innerHTML/, `${file} sets inner HTML`);
  }
});
