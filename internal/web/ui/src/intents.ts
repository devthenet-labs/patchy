// Pure helpers behind the intents views: column grouping, durations, a run
// row's link text and click rule, and the plain-English stop conditions of a
// run. No DOM, no fetches, so the node test runner exercises them directly
// (tests/intents.test.mjs).

import type {
  IntentCard,
  IntentColumn,
  IntentPhase,
  IntentRunDetail,
  IntentRunRow,
  IntentTier,
  RunActivity,
} from "./types";

export const INTENT_COLUMNS: { id: IntentColumn; label: string }[] = [
  { id: "planning", label: "Planning" },
  { id: "approval", label: "Awaiting approval" },
  { id: "building", label: "Building" },
  { id: "review", label: "In review" },
  { id: "blocked", label: "Blocked" },
  { id: "done", label: "Done" },
];

export const INTENT_PHASE_LABELS: Record<IntentPhase, string> = {
  Pending: "pending",
  Planning: "planning",
  AwaitingApproval: "awaiting approval",
  Building: "building",
  InReview: "in review",
  Revising: "revising",
  Blocked: "blocked",
  Merged: "merged",
  Closed: "closed",
  Failed: "failed",
};

// groupByColumn sorts the board's cards into their columns, the most
// recently moved first. An unknown column falls back to "blocked", the
// column for what the board cannot place.
export function groupByColumn(cards: IntentCard[]): Record<IntentColumn, IntentCard[]> {
  const out = {} as Record<IntentColumn, IntentCard[]>;
  for (const c of INTENT_COLUMNS) out[c.id] = [];
  for (const card of cards) {
    (out[card.column] ?? out.blocked).push(card);
  }
  const at = (c: IntentCard) => Date.parse(c.phaseSince ?? c.requestedAt ?? "") || 0;
  for (const c of INTENT_COLUMNS) out[c.id].sort((a, b) => at(b) - at(a) || a.name.localeCompare(b.name));
  return out;
}

// formatDuration renders seconds compactly: "45s", "4m 10s", "1h 05m",
// "2d 3h". Negative or absent values render as "—".
export function formatDuration(seconds?: number): string {
  if (seconds === undefined || !Number.isFinite(seconds) || seconds < 0) return "—";
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${String(m % 60).padStart(2, "0")}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

// secondsSince is the whole seconds from iso to now, or undefined when iso
// does not parse. A time in the future reads as zero.
export function secondsSince(iso: string | undefined, now: number): number | undefined {
  if (!iso) return undefined;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return undefined;
  return Math.max(0, Math.floor((now - t) / 1000));
}

// costShare is spend over its ceiling, clamped to [0, 1]; over is whether
// the spend passed it.
export function costShare(cost: number, ceiling: number): { share: number; over: boolean } {
  if (!(ceiling > 0)) return { share: 0, over: false };
  return { share: Math.min(1, Math.max(0, cost / ceiling)), over: cost >= ceiling };
}

// runLinkLabel is the visible text of a timeline row's link to its run
// panel: what that panel will show this viewer. The conversation is there
// only for a transcripts-tier reader, and only while the run is live or once
// it recorded one; otherwise the panel shows the run's clock, activity and
// stop conditions, so the link says no more than "Open run".
export function runLinkLabel(tier: IntentTier, run: Pick<IntentRunRow, "running" | "transcript">): string {
  return tier === "transcripts" && (run.running || run.transcript) ? "View conversation" : "Open run";
}

// RowClick is what a click on a timeline row carried, read off the DOM event
// by the component so the decision stays pure.
export interface RowClick {
  button: number;
  // modified: ctrl, meta, shift or alt was held.
  modified: boolean;
  defaultPrevented: boolean;
  // onInteractive: the click landed on, or inside, a link or a control.
  onInteractive: boolean;
  // selecting: the press left a text selection behind (a drag over a
  // digest to copy it, say).
  selecting: boolean;
}

// followsRowClick reports whether a click on a run row should open its run
// panel: a plain primary click on the row's own surface. A click on the
// row's link is the link's to handle (it navigates itself, and a modified
// click opens a tab as any link's does), so a modified click elsewhere on
// the row does nothing rather than guess; and a drag that selected text is
// not a request to navigate.
export function followsRowClick(c: RowClick): boolean {
  return c.button === 0 && !c.modified && !c.defaultPrevented && !c.onInteractive && !c.selecting;
}

function usd(micro: number): string {
  return `$${(Math.max(0, micro) / 1_000_000).toFixed(2)}`;
}

// stopConditions says, in plain English, everything that will end a run, and
// how close each is, from the run's grant, its Job's clock and its live
// activity. Every figure comes from the server; nothing here is a limit of
// its own.
export function stopConditions(run: IntentRunDetail, activity: RunActivity | null, now: number): string[] {
  const out: string[] = [];
  const started = secondsSince(run.startedAt, now);
  const timeout = run.grant?.timeoutMilliseconds ? run.grant.timeoutMilliseconds / 1000 : undefined;
  if (timeout !== undefined) {
    if (started === undefined || !run.running) {
      out.push(`stage time limit ${formatDuration(timeout)}`);
    } else if (started < timeout) {
      out.push(`stops at the ${formatDuration(timeout)} stage time limit in ${formatDuration(timeout - started)}`);
    } else {
      out.push(`past its ${formatDuration(timeout)} stage time limit: it is being stopped`);
    }
  }
  const job = run.job;
  if (job?.deadlineSeconds) {
    const since = secondsSince(job.startedAt ?? job.createdAt, now);
    out.push(
      since === undefined
        ? `the Job's deadline is ${formatDuration(job.deadlineSeconds)}`
        : `the Job's ${formatDuration(job.deadlineSeconds)} deadline ends it in ${formatDuration(Math.max(0, job.deadlineSeconds - since))}`,
    );
  }
  // Past the transcript cap the recorder records nothing more, so its last
  // time and count are frozen: they say nothing about idleness or turns.
  const capped = run.running && activity?.capped;
  if (job?.idleTimeoutSeconds) {
    let line = `ends after ${formatDuration(job.idleTimeoutSeconds)} with no model turn or tool result`;
    const idle = secondsSince(activity?.lastAt, now);
    if (capped) line += " (activity since the transcript cap is not recorded)";
    else if (run.running && idle !== undefined) line += ` (last activity ${formatDuration(idle)} ago)`;
    out.push(line);
  }
  if (run.grant?.maxTurns) {
    const turns = activity?.turns;
    out.push(
      `turn limit ${run.grant.maxTurns}` +
        (capped
          ? ` (the transcript stopped recording at ${turns} entries)`
          : turns !== undefined && run.running
            ? ` (${turns} transcript entries so far; not every entry is a turn)`
            : ""),
    );
  }
  if (run.grant?.tokenBudget) {
    out.push(
      `output-token kill switch at ${run.grant.tokenBudget.toLocaleString("en-US")} streamed tokens (not a spend bound)`,
    );
  }
  if (run.lastAttempt) {
    out.push(
      run.stage === "revise"
        ? "last attempt: if it fails, the round ends"
        : "last attempt: if it fails, the intent fails",
    );
  } else {
    out.push(`attempt ${run.countedAttempt} of ${run.limits.maxAttempts}: a failure is retried`);
  }
  out.push(
    `cost ceiling ${usd(run.limits.maxCostMicroUSD)} is checked only before the next launch ` +
      `(recorded so far ${usd(run.intentCostMicroUSD)}); this run can pass it`,
  );
  return out;
}

// INTENTS_VIEW_FILES are the components that render intent data. They must
// never import Markdown: agent text is shown as plain text only
// (tests/intents.test.mjs checks the imports).
export const INTENTS_VIEW_FILES = [
  "src/components/IntentsBoard.tsx",
  "src/components/IntentTimeline.tsx",
  "src/components/RunPanel.tsx",
  "src/components/PlainText.tsx",
  "src/components/IntentsArea.tsx",
];
