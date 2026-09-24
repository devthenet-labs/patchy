// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package command parses the one grammar every human command arriving from
// GitHub uses, and knows which verbs each GitHub surface offers.
//
// # Grammar
//
// A comment is a command when its first non-blank line, after trimming, is
//
//	/patchy <verb> [note]
//
// A line ends at every line break Unicode defines (UAX #14: CRLF, CR, LF,
// VT, FF, NEL, U+2028 and U+2029), for the command line and the note alike.
// The prefix is a whole word, matched without regard to ASCII case, and any
// other whitespace separates it from the verb. The verb is the next
// whitespace-delimited word on that line, lower-cased; a word that is not
// 1-32 ASCII letters parses as an empty verb, so a caller can still answer
// with the verbs it offers and can echo a non-empty verb without escaping it.
// The note is everything after the verb: the rest of that line and every line
// below it. Only the first non-blank line is ever a command. A later line
// that looks like one, a quoted command ("> /patchy approve"), one inside a
// code span or fenced code block, and one indented by four or more columns
// (a tab counting to the next multiple of four), which GitHub renders as an
// indented code block, are all text, and text never parses.
//
// # Notes
//
// Every note, however its command arrived, is made by one exported rule,
// Note: invalid UTF-8 replaced, every line break normalised to "\n", control
// characters other than "\n" and "\t" removed, surrounding whitespace
// trimmed, and cut to at most MaxNoteBytes on a rune boundary.
//
// A note is a human's words that other humans read on GitHub and the status
// page and that an agent may read in a prompt, so the rule also removes the
// characters that make text read differently from what it holds: bidi
// embeddings, overrides and isolates, which reorder it; tag characters,
// which spell ASCII that renders as nothing but that a model reads; U+FEFF;
// and every variation selector but one straight after a visible character,
// so a run of them cannot carry hidden bytes. Everything else is kept,
// format characters included, because ordinary text needs them: ZWJ builds
// emoji sequences, ZWNJ spells Persian and Indic words, and soft hyphens,
// bidi marks and zero-width spaces are written on purpose. The rule keeps a
// note honest about its order and its letters; it does not promise that a
// note holds nothing invisible.
//
// # Aliases
//
// On a Finding's tracking issue, and only there, the legacy approve comment
// ("/approve", or the Integration's configured
// spec.github.issues.approveComment) parses as the verb approve with
// Command.Alias set to the form used; a Parser honours it only when its
// Surface is FindingIssue, and Parse never does. Everywhere else it is text,
// so an "/approve" on an intent approves nothing, and one meant for another
// bot on an intent's pull request draws no reply. It is matched exactly as
// the Finding webhook handler has always matched it, so moving that handler
// onto this package cannot change which comments approve a Finding: the
// whole comment, trimmed, must equal the alias or start with the alias and a
// space; the match is case-sensitive; and the note is the rest of the
// comment, trailing lines included. Unlike the grammar, it therefore still
// matches an indented comment, as it always has. The one deliberate
// difference is in the note, which follows Note where the handler kept it
// verbatim apart from the 1 KiB cut. The /patchy grammar is tried first:
// where a comment parses as the grammar, a configured alias that also
// matches it is ignored.
//
// The other aliases the design names (the approve label, re-applying the
// trigger label, a "Request changes" review) are GitHub events rather than
// comment text. Their callers map them onto the same verbs by building the
// Command themselves, with any text the event carries passed through Note
// rather than dressed up as a comment for Parse: a review is
// Command{Verb: action.VerbRevise, Note: Note(review.Body)}, so a review body
// that itself opens with a command line or blank lines is still all note.
//
// # What the caller decides
//
// Parse only says what a comment asks for. Whether the verb is offered on the
// surface the comment was made on (Available, with Help for the reply to an
// unknown one), whether it means anything in the object's current phase
// (with HelpFor for the reply listing the verbs that phase admits), and
// whether the commenter may issue it are all the caller's decisions. The
// package makes no GitHub or Kubernetes call and imports neither; the verb
// names come from internal/action, the vocabulary the status page and the
// CLI share.
package command
