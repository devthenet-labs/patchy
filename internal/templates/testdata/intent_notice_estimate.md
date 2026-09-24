<!-- patchy:notice patchy/target-1 estimate-r2 -->
Plan r2 estimates that its build needs 200 turns and 500000 output tokens, but a build of this project is granted 150 turns and 800000 output tokens: the turns are over. The build runs on its grant, never on the estimate, so it may stop before the plan is built.

Approving does not raise the grant. To give the build more, raise the project's build limit, then ask for a new plan: re-apply the `patchy:target` label or comment `/patchy replan`.
