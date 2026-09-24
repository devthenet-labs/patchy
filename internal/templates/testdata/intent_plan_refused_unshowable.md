<!-- patchy:notice patchy/target-1 plan-r2 -->
## Plan r2 refused

The plan holds 26 characters that no comment can show and no plan written for a person needs: Unicode tag characters, which render as nothing and which a model reads as the text they shadow; bidi controls, which reorder the text around them; or variation selectors beyond the one an emoji takes. An approver must see all of a plan exactly as the build agent reads it, so patchy does not post part of one: plan r2 (`sha256:a5c390bc1024`) cannot be approved, and patchy will not build it.

For a new plan, say what should change in a comment, then re-apply the `patchy:target` label or comment `/patchy replan`. `/patchy cancel` stops work on this intent.
