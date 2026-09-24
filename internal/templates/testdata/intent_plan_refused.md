<!-- patchy:notice patchy/target-1 plan-r2 -->
## Plan r2 refused

The plan is too large to show here: with patchy's notes it comes to 66412 bytes, and patchy posts at most 65536, GitHub's limit for a comment. An approver must see all of a plan exactly as the build agent reads it, so patchy does not post part of one: plan r2 (`sha256:e3ec3c621917`) cannot be approved, and patchy will not build it.

For a new plan, say what should change in a comment, such as a shorter plan, then re-apply the `patchy:target` label or comment `/patchy replan`. `/patchy cancel` stops work on this intent.
