// The intents board: every intent the caller may see, in columns by phase. A
// Blocked intent stays in the column of the phase it was blocked from, with
// a badge saying why in public wording. Everything shown is a figure the
// server recorded, or a link it vetted (https, from a controller field).

import type { BoardProject, IntentBoard, IntentCard } from "../types";
import { formatAgo, formatMicroUSD } from "../format";
import { INTENT_COLUMNS, costShare, formatDuration, groupByColumn, secondsSince } from "../intents";
import { hrefForIntent, hrefForIntentRun } from "../router";
import { Icon } from "./icons";
import { Pill } from "./Pills";

function CostBar({ cost, ceiling }: { cost: number; ceiling: number }) {
  const { share, over } = costShare(cost, ceiling);
  return (
    <div class="flex items-center gap-2" title="Recorded spend against the project's cost ceiling">
      <span class="block h-[5px] flex-1 overflow-hidden rounded-full bg-line">
        <span
          class={`block h-full rounded-full ${over ? "ps-fill--red" : share > 0.75 ? "ps-fill--amber" : "ps-fill--green"}`}
          style={{ width: `${Math.round(share * 100)}%` }}
        />
      </span>
      <span class="font-mono text-[10.5px] whitespace-nowrap text-muted">
        {formatMicroUSD(cost)} / {formatMicroUSD(ceiling)}
      </span>
    </div>
  );
}

function Card({ card, project, now }: { card: IntentCard; project?: BoardProject; now: number }) {
  const limits = project?.limits;
  return (
    <li class="flex flex-col gap-2 rounded-[11px] border border-line bg-surface p-3 shadow-card">
      <div class="flex items-start justify-between gap-2">
        <a href={hrefForIntent(card.name)} class="font-mono text-[12.5px] font-semibold text-fg no-underline hover:underline">
          {card.name}
        </a>
        <span class="font-mono text-[10px] text-faint">#{card.issue}</span>
      </div>
      {card.summary ? <p class="m-0 text-[12.5px] leading-snug text-fg">{card.summary}</p> : null}
      <div class="flex flex-wrap gap-1.5">
        {card.phase === "Blocked" ? <Pill tone="red">blocked</Pill> : null}
        {card.suspended ? (
          <Pill tone="amber" title="Suspended: nothing launches, and its status may be stale">
            suspended
          </Pill>
        ) : null}
        {card.phase === "Revising" ? <Pill tone="seedling">revising</Pill> : null}
        {card.column === "done" ? <Pill tone={card.phase === "Merged" ? "green" : "soil"}>{card.phase.toLowerCase()}</Pill> : null}
      </div>
      {card.blockedReasons?.length ? (
        <ul class="m-0 list-none p-0 text-[11.5px] text-red">
          {card.blockedReasons.map((r) => (
            <li key={r}>{r}</li>
          ))}
        </ul>
      ) : null}
      {card.runningRuns?.map((run) => (
        <a
          key={run.name}
          href={hrefForIntentRun(card.name, run.name)}
          class="flex items-center gap-1.5 text-[11.5px] text-ink no-underline hover:underline"
        >
          <span class="ps-live-dot" />
          {run.stage} {run.repository ? `· ${run.repository}` : ""} · {run.phase.toLowerCase()}{" "}
          {run.startedAt ? formatDuration(secondsSince(run.startedAt, now)) : ""}
        </a>
      ))}
      <dl class="m-0 grid grid-cols-3 gap-x-2 gap-y-1 text-[10.5px]">
        <div>
          <dt class="text-faint">attempt</dt>
          <dd class="m-0 font-mono">{card.attempt ? `${card.attempt.current}/${card.attempt.max}` : "—"}</dd>
        </div>
        <div>
          <dt class="text-faint">revisions</dt>
          <dd class="m-0 font-mono">
            {card.revisions}/{limits?.maxRevisions ?? "—"}
          </dd>
        </div>
        <div>
          <dt class="text-faint">CI fixes</dt>
          <dd class="m-0 font-mono">
            {card.checkFixes}/{limits?.maxCheckFixes ?? "—"}
          </dd>
        </div>
      </dl>
      {limits ? <CostBar cost={card.costMicroUSD} ceiling={limits.maxCostMicroUSD} /> : null}
      <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-[10.5px] text-muted">
        <span title="Since the trigger label was applied">age {formatAgo(card.requestedAt, now)}</span>
        {card.phaseSince ? <span>in phase {formatAgo(card.phaseSince, now)}</span> : null}
        {card.pullRequests?.map((pr) =>
          pr.url ? (
            <a key={pr.url} href={pr.url} target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-1 text-muted">
              <Icon name="gitPullRequest" size={11} />
              {pr.repository}#{pr.number}
            </a>
          ) : (
            <span key={`${pr.repository}#${pr.number}`}>
              {pr.repository}#{pr.number}
            </span>
          ),
        )}
        {card.preview?.url ? (
          <a href={card.preview.url} target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-1 text-muted">
            <Icon name="externalLink" size={11} />
            preview
          </a>
        ) : null}
      </div>
    </li>
  );
}

export function IntentsBoard({ board, now }: { board: IntentBoard; now: number }) {
  if (board.projects.length === 0) {
    return (
      <div class="mx-auto my-20 max-w-[460px] rounded-xl border border-line-2 bg-surface p-7 text-center shadow-card">
        <h1 class="mx-0 mt-0 mb-2 text-[19px] tracking-tight">No projects to show</h1>
        <p class="m-0 text-muted">
          You are signed in, but no Project grants you its intents. Ask an operator for get on projects/intents
          (docs: the intents dashboard).
        </p>
      </div>
    );
  }
  const projects = new Map(board.projects.map((p) => [p.name, p]));
  const columns = groupByColumn(board.intents);
  return (
    <>
      <div class="mb-4 flex flex-wrap items-center gap-2">
        <h1 class="m-0 mr-2 text-[19px] tracking-tight">Intents</h1>
        {board.projects.map((p) => (
          <span key={p.name} class="ps-chip" title={p.tier === "transcripts" ? "plans and transcripts too" : "board and numbers"}>
            {p.name} · {p.tier}
            {p.suspended ? " · suspended" : ""}
          </span>
        ))}
      </div>
      <div class="grid grid-cols-[repeat(6,minmax(210px,1fr))] gap-3 overflow-x-auto pb-2">
        {INTENT_COLUMNS.map((col) => (
          <section key={col.id} aria-label={col.label} class="min-w-[210px]">
            <h2 class="ps-heading mb-2 flex items-center justify-between">
              {col.label}
              <span class="font-mono text-[10px] text-faint">{columns[col.id].length}</span>
            </h2>
            <ul class="m-0 flex list-none flex-col gap-2.5 p-0">
              {columns[col.id].map((card) => (
                <Card key={card.name} card={card} project={projects.get(card.project)} now={now} />
              ))}
            </ul>
          </section>
        ))}
      </div>
    </>
  );
}
