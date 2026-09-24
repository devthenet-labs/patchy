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
// The prefix is a whole word, matched without regard to ASCII case, and any
// whitespace separates it from the verb. The verb is the next
// whitespace-delimited word, lower-cased; a word that is not 1-32 ASCII
// letters parses as an empty verb, so a caller can still answer with the
// verbs it offers and can echo a non-empty verb without escaping it. The note
// is everything after the verb: the rest of that line and every line below
// it. Only the first non-blank line is ever a command. A later line that
// looks like one, a quoted command ("> /patchy approve") or one inside a code
// span is text, and text never parses.
//
// A note has invalid UTF-8 replaced, line breaks normalised to "\n", control
// and format characters other than "\n" and "\t" removed, surrounding
// whitespace trimmed, and is cut to at most MaxNoteBytes on a rune boundary.
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
// comment, trailing lines included. The one deliberate difference is in the
// note, which is sanitised as above where the handler kept it verbatim apart
// from the 1 KiB cut. The /patchy grammar is tried first: where a comment
// parses as the grammar, a configured alias that also matches it is ignored.
//
// The other aliases the design names (the approve label, re-applying the
// trigger label, a "Request changes" review) are GitHub events rather than
// comment text; their callers map them onto the same verbs.
//
// # What the caller decides
//
// Parse only says what a comment asks for. Whether the verb is offered on the
// surface the comment was made on (Available, with Help for the reply to an
// unknown one), whether it means anything in the object's current phase, and
// whether the commenter may issue it are all the caller's decisions. The
// package makes no GitHub or Kubernetes call and imports neither; the verb
// names come from internal/action, the vocabulary the status page and the
// CLI share.
package command
