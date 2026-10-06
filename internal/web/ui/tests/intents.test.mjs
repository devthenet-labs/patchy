// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import {
  INTENTS_VIEW_FILES,
  costShare,
  formatDuration,
  groupByColumn,
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

test("stopConditions for a revise round's earlier attempt", () => {
  const lines = stopConditions({ ...run, stage: "revise", lastAttempt: false, attempt: 1, job: undefined }, null, now);
  assert.ok(lines.includes("attempt 1 of 2: a failure is retried"));
  assert.ok(!lines.some((l) => l.includes("deadline")));
  const last = stopConditions({ ...run, stage: "revise" }, null, now);
  assert.ok(last.includes("last attempt: if it fails, the round ends"));
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

// Agent text is rendered as plain text only: no intents view may reach the
// markdown renderer, or put raw HTML into the page.
test("no intents view imports Markdown or sets inner HTML", () => {
  for (const file of INTENTS_VIEW_FILES) {
    const src = readFileSync(new URL(`../${file}`, import.meta.url), "utf8");
    assert.doesNotMatch(src, /from\s+["']\.\/Markdown["']/, `${file} imports Markdown`);
    assert.doesNotMatch(src, /dangerouslySetInnerHTML|innerHTML/, `${file} sets inner HTML`);
  }
});
