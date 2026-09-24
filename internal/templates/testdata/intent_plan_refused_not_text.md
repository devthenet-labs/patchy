<!-- patchy:notice patchy/target-1 plan-r2 -->
## Plan r2 refused

The plan is not UTF-8 text, and a comment holds only text, so no comment can show it as the build agent would read it. An approver must see all of a plan exactly as the build agent reads it, so patchy does not post part of one: plan r2 (`sha256:565a8ec7adb0`) cannot be approved, and patchy will not build it.

For a new plan, say what should change in a comment, then re-apply the `patchy:target` label or comment `/patchy replan`. `/patchy cancel` stops work on this intent.
