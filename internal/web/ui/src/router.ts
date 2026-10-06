// Hash routing — works from a static file server, over file://, and inside
// the embedded server without rewrites.
//
//   #/                    findings list
//   #/finding/{name}      finding detail (optional /{tab})
//   #/rollups             rollups, optional /{scope}
//   #/config              configuration, optional /{section}
//   #/intents             intents board (when the server has the views on)
//   #/intents/{name}      one intent's timeline
//   #/intents/{name}/runs/{run}  one run's panel

import { useEffect, useState } from "preact/hooks";
import type { ScopeType } from "./types";

export type TabId = "overview" | "alerts" | "timeline" | "investigation" | "remediation";

export type ConfigSection = "forges" | "integrations" | "enhancers";

export type Route =
  | { view: "list" }
  | { view: "detail"; name: string; tab: TabId }
  | { view: "rollups"; scope: ScopeType }
  | { view: "config"; section: ConfigSection }
  | { view: "intents" }
  | { view: "intent"; name: string }
  | { view: "intentRun"; name: string; run: string };

// isIntentsRoute reports a route of the intents views, which gate
// themselves rather than on the findings grant.
export function isIntentsRoute(route: Route): boolean {
  return route.view === "intents" || route.view === "intent" || route.view === "intentRun";
}

const TABS: TabId[] = ["overview", "alerts", "timeline", "investigation", "remediation"];
const SCOPES: ScopeType[] = ["total", "repository", "harness", "model"];
const SECTIONS: ConfigSection[] = ["forges", "integrations", "enhancers"];

export function parseRoute(hash: string): Route {
  const parts = hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
  if (parts[0] === "finding" && parts[1]) {
    const tab = TABS.includes(parts[2] as TabId) ? (parts[2] as TabId) : "overview";
    return { view: "detail", name: parts[1], tab };
  }
  if (parts[0] === "rollups") {
    const scope = SCOPES.includes(parts[1] as ScopeType) ? (parts[1] as ScopeType) : "total";
    return { view: "rollups", scope };
  }
  if (parts[0] === "config") {
    const section = SECTIONS.includes(parts[1] as ConfigSection)
      ? (parts[1] as ConfigSection)
      : "integrations";
    return { view: "config", section };
  }
  if (parts[0] === "intents") {
    if (parts[1] && parts[2] === "runs" && parts[3]) {
      return { view: "intentRun", name: parts[1], run: parts[3] };
    }
    if (parts[1]) return { view: "intent", name: parts[1] };
    return { view: "intents" };
  }
  return { view: "list" };
}

export function hrefForIntents(): string {
  return "#/intents";
}

export function hrefForIntent(name: string): string {
  return `#/intents/${encodeURIComponent(name)}`;
}

export function hrefForIntentRun(name: string, run: string): string {
  return `#/intents/${encodeURIComponent(name)}/runs/${encodeURIComponent(run)}`;
}

export function hrefForList(): string {
  return "#/";
}

export function hrefForFinding(name: string, tab?: TabId): string {
  const base = `#/finding/${encodeURIComponent(name)}`;
  return tab && tab !== "overview" ? `${base}/${tab}` : base;
}

export function hrefForRollups(scope?: ScopeType): string {
  return scope && scope !== "total" ? `#/rollups/${scope}` : "#/rollups";
}

export function hrefForConfig(section?: ConfigSection): string {
  return section && section !== "integrations" ? `#/config/${section}` : "#/config";
}

export function navigate(href: string): void {
  window.location.hash = href.replace(/^#/, "");
}

export function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() => parseRoute(window.location.hash));
  useEffect(() => {
    const onChange = () => setRoute(parseRoute(window.location.hash));
    window.addEventListener("hashchange", onChange);
    return () => window.removeEventListener("hashchange", onChange);
  }, []);
  return route;
}
