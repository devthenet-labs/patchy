// One run's panel: what it is doing now and everything that will stop it,
// in plain English, from the server's figures. Every tier sees the activity
// (turn count, the open tool and for how long, the last activity); a
// transcripts-tier reader also gets the report and the conversation, as
// plain text. The live stream is stripped server-side: a viewer without the
// tier never receives the text at all.

import { useEffect, useState } from "preact/hooks";
import type { IntentRunDetail, RunActivity, TranscriptTurn } from "../types";
import { streamRun } from "../api";
import { formatDate, formatMicroUSD, formatTokens } from "../format";
import { formatDuration, secondsSince, stopConditions } from "../intents";
import { hrefForIntent } from "../router";
import { Icon } from "./icons";
import { PlainText } from "./PlainText";
import { Pill } from "./Pills";

function useRunStream(run: IntentRunDetail) {
  const [activity, setActivity] = useState<RunActivity | null>(null);
  const [turns, setTurns] = useState<TranscriptTurn[]>([]);
  const [notice, setNotice] = useState<string | null>(null);
  const [ended, setEnded] = useState<string | null>(null);
  useEffect(() => {
    setActivity(null);
    setTurns([]);
    setNotice(null);
    setEnded(null);
    return streamRun(run.intent, run.name, {
      onActivity: setActivity,
      onTurn: (t) => setTurns((prev) => [...prev, t]),
      onUnavailable: setNotice,
      onEnd: (reason) => setEnded(reason),
    });
  }, [run.intent, run.name]);
  return { activity, turns, notice, ended };
}

function turnText(t: TranscriptTurn): string {
  const head = `[${t.kind}${t.tool ? ` ${t.tool}` : ""}${t.at ? ` ${t.at}` : ""}]`;
  return `${head}\n${t.text ?? ""}${t.truncated ? "\n(truncated by the recorder)" : ""}`;
}

export function RunPanel({ run, now }: { run: IntentRunDetail; now: number }) {
  const { activity, turns, notice, ended } = useRunStream(run);
  const elapsed = run.startedAt
    ? run.finishedAt
      ? Math.max(0, Math.floor((Date.parse(run.finishedAt) - Date.parse(run.startedAt)) / 1000))
      : secondsSince(run.startedAt, now)
    : undefined;
  const conditions = stopConditions(run, activity, now);
  return (
    <div class="pb-6">
      <a href={hrefForIntent(run.intent)} class="mb-3 inline-flex items-center gap-1.5 text-[12px] text-muted no-underline">
        <Icon name="arrowLeft" size={13} /> {run.intent}
      </a>
      <div class="flex flex-wrap items-center gap-3">
        <h1 class="m-0 font-mono text-[18px] tracking-tight">{run.name}</h1>
        {run.running ? (
          <Pill tone="seedling">
            <span class="ps-live-dot" /> {run.phase?.toLowerCase()}
          </Pill>
        ) : (
          <Pill tone={run.outcome === "ok" ? "green" : run.outcome ? "red" : "neutral"}>{run.outcome ?? run.phase ?? "—"}</Pill>
        )}
        <span class="ps-chip">
          {run.stage} r{run.round} attempt {run.countedAttempt}/{run.limits.maxAttempts}
          {run.trigger ? ` · ${run.trigger}` : ""}
        </span>
      </div>
      {run.reason && !run.running ? <p class="mt-2 mb-0 text-[13px] text-muted">{run.reason}</p> : null}

      <dl class="ps-kv mt-5">
        <div>
          <dt>Repository</dt>
          <dd>{run.repository ?? "—"}</dd>
        </div>
        <div>
          <dt>Elapsed</dt>
          <dd class="font-mono">
            {formatDuration(elapsed)}
            {run.grant?.timeoutMilliseconds ? ` of ${formatDuration(run.grant.timeoutMilliseconds / 1000)}` : ""}
          </dd>
        </div>
        <div>
          <dt>Activity</dt>
          <dd class="font-mono">
            {activity ? `${activity.turns} entries` : "—"}
            {run.grant?.maxTurns ? ` · ${run.grant.maxTurns} turn limit` : ""}
          </dd>
        </div>
        <div>
          <dt>Now</dt>
          <dd>
            {activity?.live && activity.openTool ? (
              <>
                running <span class="ps-mono-tag">{activity.openTool}</span> for{" "}
                <span class="font-mono">{formatDuration(secondsSince(activity.openToolSince, now))}</span>
              </>
            ) : activity?.live ? (
              "thinking or between tools"
            ) : run.running ? (
              "—"
            ) : (
              "finished"
            )}
          </dd>
        </div>
        <div>
          <dt>Last activity</dt>
          <dd>
            {activity?.lastAt ? `${formatDate(activity.lastAt)} (${formatDuration(secondsSince(activity.lastAt, now))} ago)` : "—"}
          </dd>
        </div>
        <div>
          <dt>Recorded cost</dt>
          <dd class="font-mono">
            {run.costMicroUSD ? formatMicroUSD(run.costMicroUSD) : run.running ? "recorded when it ends" : "—"}
            {run.usage?.outputTokens ? ` · ${formatTokens(run.usage.outputTokens)} out` : ""}
          </dd>
        </div>
        <div>
          <dt>Image</dt>
          <dd class="font-mono">
            {run.imageSource ?? "—"}
            {run.imageDigest ? ` · ${run.imageDigest}` : ""}
          </dd>
        </div>
        <div>
          <dt>Commits</dt>
          <dd class="font-mono">
            {run.baseSHA ? `base ${run.baseSHA.slice(0, 12)}` : "—"}
            {run.pushedCommit ? ` · pushed ${run.pushedCommit.slice(0, 12)}` : ""}
          </dd>
        </div>
      </dl>

      {notice ? (
        <div class="ps-note">
          <Icon name="alertTriangle" size={14} /> {notice}
        </div>
      ) : null}
      {ended === "revoked" ? (
        <div class="ps-note ps-note--red">
          <Icon name="alertTriangle" size={14} /> Your access to this project changed; the stream stopped.
        </div>
      ) : null}

      <section class="mt-6">
        <h2 class="ps-heading mb-2">What stops it</h2>
        <ul class="m-0 flex list-disc flex-col gap-1 pl-5 text-[12.5px]">
          {conditions.map((c) => (
            <li key={c}>{c}</li>
          ))}
        </ul>
      </section>

      {run.tier === "transcripts" ? (
        <>
          {run.report ? (
            <section class="mt-6">
              <h2 class="ps-heading mb-2">Report</h2>
              <PlainText label="written by the agent" text={run.report} />
            </section>
          ) : null}
          <section class="mt-6">
            <h2 class="ps-heading mb-2">Conversation</h2>
            {turns.length === 0 ? (
              <p class="text-faint">{run.running ? "Waiting for the agent…" : "No conversation recorded."}</p>
            ) : (
              <PlainText label="written by the agent and its tools" text={turns.map(turnText).join("\n\n")} />
            )}
          </section>
        </>
      ) : (
        <p class="mt-6 text-[12px] text-faint">
          Plans, reports and transcripts need the transcripts tier on this project.
        </p>
      )}
    </div>
  );
}
