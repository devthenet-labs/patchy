// PlainText shows agent- or human-written text exactly as text: one text
// node in a pre-wrapped block, no markdown, no link detection, nothing a
// string can turn into markup. The server has already made every invisible
// character visible ([U+XXXX]). The frame says who wrote it, so a plan that
// claims "approved by @owner" cannot pass for patchy's own words.

export function PlainText({ text, label, maxHeight = true }: { text: string; label: string; maxHeight?: boolean }) {
  return (
    <figure class="m-0 rounded-[11px] border border-line bg-code">
      <figcaption class="flex items-center gap-2 border-b border-line px-3.5 py-2 font-mono text-[10px] tracking-[0.07em] text-faint uppercase">
        {label}
      </figcaption>
      <pre
        class={`m-0 overflow-x-auto px-3.5 py-3 font-mono text-[12px] leading-relaxed whitespace-pre-wrap break-words text-fg ${
          maxHeight ? "max-h-[36rem] overflow-y-auto" : ""
        }`}
      >
        {text}
      </pre>
    </figure>
  );
}
