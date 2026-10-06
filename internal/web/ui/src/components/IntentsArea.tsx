// The intents views' container: it loads what the route names, refetches on
// the intents change signal, and renders the sign-in, not-found and error
// states itself. It gates on its own answers (401, 404), never on the
// findings grant: a viewer of intents may see no findings at all.

import type { ComponentChildren } from "preact";
import { useCallback, useEffect, useState } from "preact/hooks";
import type { IntentBoard, IntentDetail, IntentRunDetail } from "../types";
import { AuthRequiredError, fetchIntent, fetchIntentBoard, fetchIntentRun, subscribeIntents } from "../api";
import { signInURL } from "../auth";
import type { Route } from "../router";
import { hrefForIntents } from "../router";
import { IntentsBoard } from "./IntentsBoard";
import { IntentTimeline } from "./IntentTimeline";
import { RunPanel } from "./RunPanel";

type Loaded =
  | { kind: "loading" }
  | { kind: "auth" }
  | { kind: "missing" }
  | { kind: "error"; message: string }
  | { kind: "board"; board: IntentBoard }
  | { kind: "intent"; detail: IntentDetail }
  | { kind: "run"; run: IntentRunDetail };

function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(t);
  }, []);
  return now;
}

function Panel({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <div class="mx-auto my-20 max-w-[460px] rounded-xl border border-line-2 bg-surface p-7 text-center shadow-card">
      <h1 class="mx-0 mt-0 mb-2 text-[19px] tracking-tight">{title}</h1>
      <div class="text-muted">{children}</div>
    </div>
  );
}

export function IntentsArea({ route }: { route: Route }) {
  const [state, setState] = useState<Loaded>({ kind: "loading" });
  const now = useNow();
  const key = route.view === "intentRun" ? `${route.name}/${route.run}` : route.view === "intent" ? route.name : "";

  const load = useCallback(async () => {
    try {
      if (route.view === "intents") {
        setState({ kind: "board", board: await fetchIntentBoard() });
      } else if (route.view === "intent") {
        const detail = await fetchIntent(route.name);
        setState(detail ? { kind: "intent", detail } : { kind: "missing" });
      } else if (route.view === "intentRun") {
        const run = await fetchIntentRun(route.name, route.run);
        setState(run ? { kind: "run", run } : { kind: "missing" });
      }
    } catch (e) {
      if (e instanceof AuthRequiredError) setState({ kind: "auth" });
      else setState({ kind: "error", message: e instanceof Error ? e.message : String(e) });
    }
    // The route's view and names are all load reads from it.
  }, [route.view, key]);

  useEffect(() => {
    setState({ kind: "loading" });
    void load();
    return subscribeIntents(() => void load());
  }, [load]);

  switch (state.kind) {
    case "loading":
      return <div class="px-5 py-11 text-center text-muted">Loading…</div>;
    case "auth":
      return (
        <Panel title="Authentication required">
          <p class="mt-0">Sign in to see intents.</p>
          <a class="ps-action ps-action--primary no-underline" href={signInURL()}>
            Sign in
          </a>
        </Panel>
      );
    case "missing":
      return (
        <Panel title="Not found">
          <p class="mt-0">
            Nothing by that name that you can see. Completed intents expire after their TTL.
          </p>
          <a href={hrefForIntents()}>Back to the board</a>
        </Panel>
      );
    case "error":
      return <Panel title="Cannot load intents">{state.message}</Panel>;
    case "board":
      return <IntentsBoard board={state.board} now={now} />;
    case "intent":
      return <IntentTimeline detail={state.detail} now={now} />;
    case "run":
      return <RunPanel run={state.run} now={now} />;
  }
}
