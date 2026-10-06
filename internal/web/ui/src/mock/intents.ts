// Demo data for the intents views (demo kit builds). Live mode never reads
// it. Shapes mirror the wire types exactly, so a field added to types.ts
// shows up here as a type error.

import type {
  IntentBoard,
  IntentCard,
  IntentDetail,
  IntentLimits,
  IntentPlanText,
  IntentRunDetail,
  IntentRunRow,
  Me,
  TranscriptTurn,
} from "../types";

const LIMITS: IntentLimits = { maxRevisions: 3, maxCheckFixes: 2, maxCostMicroUSD: 10_000_000, maxAttempts: 2 };

function ago(minutes: number): string {
  return new Date(Date.now() - minutes * 60_000).toISOString();
}

function cards(): IntentCard[] {
  return [
    {
      name: "storefront-12",
      project: "storefront",
      issue: 12,
      phase: "Building",
      column: "building",
      summary: "Add a health endpoint with a readiness probe",
      repositories: ["acme/storefront"],
      runningRuns: [
        {
          name: "storefront-12-bld-r1-web-a1",
          stage: "build",
          repository: "acme/storefront",
          round: 1,
          attempt: 1,
          phase: "Running",
          startedAt: ago(23),
        },
      ],
      attempt: { stage: "build", current: 1, max: 2 },
      revisions: 0,
      checkFixes: 0,
      costMicroUSD: 1_840_000,
      requestedBy: "octocat",
      requestedAt: ago(140),
      phaseSince: ago(24),
    },
    {
      name: "storefront-9",
      project: "storefront",
      issue: 9,
      phase: "AwaitingApproval",
      column: "approval",
      summary: "Paginate the order history",
      repositories: ["acme/storefront"],
      revisions: 0,
      checkFixes: 0,
      costMicroUSD: 620_000,
      requestedBy: "octocat",
      requestedAt: ago(300),
      phaseSince: ago(180),
    },
    {
      name: "storefront-7",
      project: "storefront",
      issue: 7,
      phase: "Blocked",
      column: "review",
      blockedFrom: "InReview",
      blockedReasons: ["the revision limit is reached (3 of 3)"],
      summary: "Rename the cart API",
      pullRequests: [{ repository: "acme/storefront", number: 41, state: "open" }],
      preview: { phase: "Ready", revision: "3f2a9c1d0b7e" },
      revisions: 3,
      checkFixes: 1,
      costMicroUSD: 7_310_000,
      requestedBy: "hubot",
      requestedAt: ago(4000),
      phaseSince: ago(900),
    },
    {
      name: "storefront-3",
      project: "storefront",
      issue: 3,
      phase: "Merged",
      column: "done",
      summary: "Fix the currency rounding",
      pullRequests: [{ repository: "acme/storefront", number: 33, state: "merged", mergedAt: ago(2000) }],
      revisions: 1,
      checkFixes: 0,
      costMicroUSD: 2_100_000,
      requestedBy: "octocat",
      requestedAt: ago(9000),
      phaseSince: ago(2000),
      completedAt: ago(2000),
    },
  ];
}

export function mockMe(): Me {
  return { name: "demo", loggedIn: true, projects: [{ name: "storefront", tier: "transcripts" }] };
}

export function mockIntentBoard(): IntentBoard {
  return {
    generatedAt: new Date().toISOString(),
    projects: [
      {
        name: "storefront",
        tier: "transcripts",
        repositories: [{ key: "web", slug: "acme/storefront" }],
        limits: LIMITS,
      },
    ],
    intents: cards(),
  };
}

function runs(card: IntentCard): IntentRunRow[] {
  const plan: IntentRunRow = {
    name: `${card.name}-plan-r1-a1`,
    stage: "plan",
    repository: "acme/storefront",
    round: 1,
    attempt: 1,
    phase: "Complete",
    outcome: "ok",
    reason: "ok: completed",
    createdAt: ago(120),
    startedAt: ago(119),
    finishedAt: ago(104),
    costMicroUSD: 620_000,
    usage: { inputTokens: 210_000, outputTokens: 9_800, costMicroUSD: 620_000 },
    imageSource: "default",
    transcript: { turns: 4 },
    grant: { maxTurns: 80, tokenBudget: 400_000, timeoutMilliseconds: 1_800_000 },
  };
  if (card.phase !== "Building") return [plan];
  return [
    plan,
    {
      name: `${card.name}-bld-r1-web-a1`,
      stage: "build",
      repository: "acme/storefront",
      round: 1,
      attempt: 1,
      phase: "Running",
      createdAt: ago(24),
      startedAt: ago(23),
      imageSource: "repository",
      imageDigest: "9c1e7a42b3d0",
      grant: { maxTurns: 150, tokenBudget: 800_000, timeoutMilliseconds: 3_600_000 },
      running: true,
    },
  ];
}

export function mockIntentDetail(name: string): IntentDetail | null {
  const card = cards().find((c) => c.name === name);
  if (!card) return null;
  return {
    ...card,
    tier: "transcripts",
    limits: LIMITS,
    phaseTimes: [
      { phase: "Pending", at: card.requestedAt ?? ago(140) },
      { phase: "Planning", at: ago(120) },
      { phase: "AwaitingApproval", at: ago(104) },
      ...(card.phase === "Building" ? [{ phase: "Building" as const, at: ago(24) }] : []),
    ],
    input: { revision: 1, digest: `sha256:${"a".repeat(64)}` },
    plan: { revision: 1, digest: `sha256:${"b".repeat(64)}`, summary: card.summary, postedAt: ago(104) },
    approval:
      card.phase === "Building"
        ? {
            by: "octocat",
            source: "label",
            at: ago(25),
            planRevision: 1,
            planDigest: `sha256:${"b".repeat(64)}`,
            inputDigest: `sha256:${"a".repeat(64)}`,
          }
        : undefined,
    runs: runs(card),
    planTexts: [1],
  };
}

export function mockIntentRun(intent: string, run: string): IntentRunDetail | null {
  const detail = mockIntentDetail(intent);
  const row = detail?.runs.find((r) => r.name === run);
  if (!detail || !row) return null;
  return {
    ...row,
    intent,
    project: detail.project,
    tier: "transcripts",
    lastAttempt: false,
    limits: LIMITS,
    intentCostMicroUSD: detail.costMicroUSD,
    job: row.running
      ? { createdAt: ago(24), startedAt: ago(23), deadlineSeconds: 5400, idleTimeoutSeconds: 1200 }
      : undefined,
    report: row.stage === "plan" ? "# Plan\n\nAdd GET /healthz returning 200 once the store is reachable.\n" : undefined,
  };
}

export function mockIntentPlan(intent: string, revision: number): IntentPlanText | null {
  if (!mockIntentDetail(intent)) return null;
  return {
    intent,
    revision,
    current: true,
    digest: `sha256:${"b".repeat(64)}`,
    text: "---\nsummary: Add a health endpoint\n---\n# Plan\n\n1. Add GET /healthz.\n2. Probe the store.\n",
  };
}

export function mockRunTurns(): TranscriptTurn[] {
  return [
    { seq: 1, at: ago(22), role: "assistant", kind: "text", text: "Reading the router to find where routes register." },
    { seq: 2, at: ago(21), role: "assistant", kind: "tool_use", tool: "Read", text: "internal/http/router.go" },
    { seq: 3, at: ago(21), role: "user", kind: "tool_result", text: "package http …" },
    { seq: 4, at: ago(9), role: "assistant", kind: "tool_use", tool: "Bash", text: "npm run test:ci" },
  ];
}
