// LiveOutput shows what the command the agent is running prints, as it
// prints it: the latest command only, live only (it is never stored, so a
// reload or a finished run shows none), and only to a transcripts-tier
// reader, the only one the server sends it to. The lines are text nodes in
// a pre, like every other agent-produced string on these views, framed as
// the command's own words. Where line numbers jump, a marker says how many
// lines are not shown. The box follows the newest line unless the reader
// has scrolled up to read back.

import { useEffect, useRef } from "preact/hooks";
import { outputSegments, outputStatus, type RunOutputState } from "../intents";
import { Pill } from "./Pills";

export function LiveOutput({ output, following }: { output: RunOutputState; following: boolean }) {
  const box = useRef<HTMLPreElement>(null);
  // Only follow the tail when the reader is already at it; yanking the view
  // away from someone reading back is worse than not following.
  const pinned = useRef(true);

  useEffect(() => {
    pinned.current = true;
  }, [output.task]);

  useEffect(() => {
    const el = box.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [output]);

  const onScroll = () => {
    const el = box.current;
    if (el) pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  };

  const status = outputStatus(output, following);
  const segments = outputSegments(output);
  return (
    <figure class="m-0 rounded-[11px] border border-line bg-code">
      <figcaption class="flex items-center gap-2 border-b border-line px-3.5 py-2 font-mono text-[10px] tracking-[0.07em] text-faint uppercase">
        printed by the command
        <span class="ml-auto normal-case tracking-normal">
          {status === "running" ? (
            <Pill tone="seedling">
              <span class="ps-live-dot" /> running
            </Pill>
          ) : (
            <Pill tone={status === "finished" ? "green" : "amber"}>{status}</Pill>
          )}
        </span>
      </figcaption>
      <pre
        ref={box}
        onScroll={onScroll}
        class="m-0 max-h-[24rem] overflow-auto px-3.5 py-3 font-mono text-[12px] leading-relaxed whitespace-pre-wrap break-words text-fg"
      >
        {segments.length === 0 ? (
          <span class="text-faint">Nothing printed yet.</span>
        ) : (
          segments.map((s, i) =>
            s.kind === "lines" ? (
              <span key={i}>
                {s.text}
                {"\n"}
              </span>
            ) : (
              <span key={i} class="text-faint italic">
                {`… ${s.count} ${s.count === 1 ? "line" : "lines"} not shown`}
                {"\n"}
              </span>
            ),
          )
        )}
      </pre>
    </figure>
  );
}
