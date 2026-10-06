// One intent's story: the issue, each plan and its approval (bound to its
// digests), every run with its outcome in public wording and its recorded
// cost, the pull requests and the preview. A transcripts-tier reader can
// also open each plan revision's text, as plain text.

import { useEffect, useState } from "preact/hooks";
import type { IntentDetail, IntentPlanText } from "../types";
import { fetchIntentPlan } from "../api";
import { formatDate, formatMicroUSD } from "../format";
import { INTENT_PHASE_LABELS, formatDuration, secondsSince } from "../intents";
import { hrefForIntentRun, hrefForIntents } from "../router";
import { Icon } from "./icons";
import { PlainText } from "./PlainText";
import { Pill } from "./Pills";

function short(digest?: string): string {
  if (!digest) return "—";
  return digest.replace(/^sha256:/, "").slice(0, 12);
}

function runSeconds(started?: string, finished?: string, now = Date.now()): number | undefined {
  if (!started) return undefined;
  const end = finished ? Date.parse(finished) : now;
  const s = Date.parse(started);
  if (Number.isNaN(s) || Number.isNaN(end)) return undefined;
  return Math.max(0, Math.floor((end - s) / 1000));
}

function PlanText({ intent, revisions }: { intent: string; revisions: number[] }) {
  const [rev, setRev] = useState<number>(revisions[revisions.length - 1]);
  const [plan, setPlan] = useState<IntentPlanText | null | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    setPlan(undefined);
    setError(null);
    fetchIntentPlan(intent, rev)
      .then(setPlan)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [intent, rev]);
  return (
    <section class="mt-6">
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <h2 class="ps-heading">Plan text</h2>
        <div class="ps-filter-group" role="group" aria-label="Plan revision">
          {revisions.map((r) => (
            <button key={r} type="button" class={r === rev ? "is-active" : ""} onClick={() => setRev(r)}>
              r{r}
            </button>
          ))}
        </div>
      </div>
      {error ? <p class="text-red">{error}</p> : null}
      {plan === undefined && !error ? <p class="text-faint">Loading the plan…</p> : null}
      {plan === null ? <p class="text-faint">Plan r{rev} was not recorded, or is no longer stored.</p> : null}
      {plan ? (
        <PlainText
          label={`plan r${plan.revision} · written by the planning agent${plan.digest ? ` · sha256 ${short(plan.digest)}` : ""}`}
          text={plan.text}
        />
      ) : null}
    </section>
  );
}

export function IntentTimeline({ detail, now }: { detail: IntentDetail; now: number }) {
  const d = detail;
  return (
    <div class="pb-6">
      <a href={hrefForIntents()} class="mb-3 inline-flex items-center gap-1.5 text-[12px] text-muted no-underline">
        <Icon name="arrowLeft" size={13} /> Intents
      </a>
      <div class="flex flex-wrap items-center gap-3">
        <h1 class="m-0 font-mono text-[20px] tracking-tight">{d.name}</h1>
        <Pill tone={d.phase === "Blocked" || d.phase === "Failed" ? "red" : d.phase === "Merged" ? "green" : "seedling"}>
          {INTENT_PHASE_LABELS[d.phase] ?? d.phase}
        </Pill>
        {d.suspended ? <Pill tone="amber">suspended</Pill> : null}
        <span class="ps-chip">
          {d.project} · {d.tier}
        </span>
      </div>
      {d.summary ? <p class="mt-2 mb-0 text-[14px]">{d.summary}</p> : null}
      {d.blockedReasons?.length ? (
        <div class="ps-note ps-note--red">
          <Icon name="alertTriangle" size={14} />
          Blocked{d.blockedFrom ? ` (from ${INTENT_PHASE_LABELS[d.blockedFrom] ?? d.blockedFrom})` : ""}:{" "}
          {d.blockedReasons.join("; ")}
        </div>
      ) : null}

      <dl class="ps-kv mt-5">
        <div>
          <dt>Issue</dt>
          <dd>
            {d.issueURL ? (
              <a href={d.issueURL} target="_blank" rel="noopener noreferrer">
                #{d.issue}
              </a>
            ) : (
              `#${d.issue}`
            )}
          </dd>
        </div>
        <div>
          <dt>Requested by</dt>
          <dd>
            {d.requestedBy ?? "—"} · {formatDate(d.requestedAt)}
          </dd>
        </div>
        <div>
          <dt>Spend (recorded)</dt>
          <dd class="font-mono">
            {formatMicroUSD(d.costMicroUSD)} of {formatMicroUSD(d.limits.maxCostMicroUSD)}
          </dd>
        </div>
        <div>
          <dt>Revisions · CI fixes</dt>
          <dd class="font-mono">
            {d.revisions}/{d.limits.maxRevisions} · {d.checkFixes}/{d.limits.maxCheckFixes}
          </dd>
        </div>
        <div>
          <dt>Plan</dt>
          <dd>
            {d.plan ? (
              <>
                r{d.plan.revision} · <span class="font-mono">{short(d.plan.digest)}</span>
                {d.plan.commentURL ? (
                  <>
                    {" "}
                    ·{" "}
                    <a href={d.plan.commentURL} target="_blank" rel="noopener noreferrer">
                      on the issue
                    </a>
                  </>
                ) : null}
              </>
            ) : (
              "—"
            )}
          </dd>
        </div>
        <div>
          <dt>Approval</dt>
          <dd>
            {d.approval ? (
              <>
                {d.approval.by} by {d.approval.source} · r{d.approval.planRevision} · plan{" "}
                <span class="font-mono">{short(d.approval.planDigest)}</span> · input{" "}
                <span class="font-mono">{short(d.approval.inputDigest)}</span>
              </>
            ) : (
              "none yet (approval happens on GitHub)"
            )}
          </dd>
        </div>
        <div>
          <dt>Pull requests</dt>
          <dd>
            {d.pullRequests?.length
              ? d.pullRequests.map((pr) => (
                  <div key={`${pr.repository}#${pr.number}`}>
                    {pr.url ? (
                      <a href={pr.url} target="_blank" rel="noopener noreferrer">
                        {pr.repository}#{pr.number}
                      </a>
                    ) : (
                      `${pr.repository}#${pr.number}`
                    )}{" "}
                    · {pr.state ?? "—"}
                  </div>
                ))
              : "—"}
          </dd>
        </div>
        <div>
          <dt>Preview</dt>
          <dd>
            {d.preview ? (
              <>
                {d.preview.phase ?? "—"}
                {d.preview.revision ? <> · <span class="font-mono">{d.preview.revision}</span></> : null}
                {d.preview.url ? (
                  <>
                    {" "}
                    ·{" "}
                    <a href={d.preview.url} target="_blank" rel="noopener noreferrer">
                      open
                    </a>
                  </>
                ) : null}
              </>
            ) : (
              "—"
            )}
          </dd>
        </div>
      </dl>

      <section class="mt-6">
        <h2 class="ps-heading mb-2">Phases</h2>
        <ol class="m-0 flex list-none flex-wrap gap-2 p-0">
          {(d.phaseTimes ?? []).map((pt, i) => (
            <li key={`${pt.phase}-${i}`} class="ps-chip" title={formatDate(pt.at)}>
              {INTENT_PHASE_LABELS[pt.phase] ?? pt.phase} · {formatDate(pt.at)}
            </li>
          ))}
        </ol>
      </section>

      <section class="mt-6">
        <h2 class="ps-heading mb-2">Runs</h2>
        <div class="overflow-x-auto rounded-[11px] border border-line bg-surface">
          <table class="w-full border-collapse text-[12px]">
            <thead>
              <tr class="text-left font-mono text-[9.5px] tracking-[0.07em] text-faint uppercase">
                <th class="px-3 py-2">Run</th>
                <th class="px-3 py-2">Stage</th>
                <th class="px-3 py-2">Repository</th>
                <th class="px-3 py-2">Outcome</th>
                <th class="px-3 py-2">Duration</th>
                <th class="px-3 py-2">Cost</th>
                <th class="px-3 py-2">Image</th>
              </tr>
            </thead>
            <tbody>
              {d.runs.map((run) => (
                <tr key={run.name} class="ps-hover-row">
                  <td class="px-3 py-2 font-mono">
                    <a href={hrefForIntentRun(d.name, run.name)}>{run.name}</a>
                  </td>
                  <td class="px-3 py-2">
                    {run.stage} r{run.round} a{run.attempt}
                    {run.trigger ? ` (${run.trigger})` : ""}
                  </td>
                  <td class="px-3 py-2">{run.repository ?? "—"}</td>
                  <td class="px-3 py-2">
                    {run.running ? (
                      <span class="inline-flex items-center gap-1.5 text-ink">
                        <span class="ps-live-dot" /> {run.phase?.toLowerCase()}
                      </span>
                    ) : (
                      (run.reason ?? run.phase ?? "—")
                    )}
                  </td>
                  <td class="px-3 py-2 font-mono">{formatDuration(runSeconds(run.startedAt, run.finishedAt, now))}</td>
                  <td class="px-3 py-2 font-mono">{run.costMicroUSD ? formatMicroUSD(run.costMicroUSD) : "—"}</td>
                  <td class="px-3 py-2 font-mono" title="digest only; the registry is not shown">
                    {run.imageSource ?? "—"}
                    {run.imageDigest ? ` ${run.imageDigest}` : ""}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {d.completedAt ? (
          <p class="mt-2 text-[11.5px] text-faint">
            Completed {formatDate(d.completedAt)} ({formatDuration(secondsSince(d.completedAt, now))} ago); completed
            intents and their runs expire after the controller's TTL (14 days by default).
          </p>
        ) : null}
      </section>

      {d.tier === "transcripts" && d.planTexts?.length ? <PlanText intent={d.name} revisions={d.planTexts} /> : null}
    </div>
  );
}
