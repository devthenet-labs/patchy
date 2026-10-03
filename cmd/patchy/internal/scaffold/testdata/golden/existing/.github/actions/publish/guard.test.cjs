'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {validate, parseID, declaredTag} = require('./guard.cjs');
const sha = 'a'.repeat(40);
const BRANCH = 'main';
const REPO_ID = 1234567890;
const OWNER_ID = 987654321;
const RUNTIME_WORKFLOW = '.github/workflows/runtime-image.yml';
const AGENT_WORKFLOW = '.github/workflows/agent-image.yml';
const AGENT_REPOSITORY = '123456789012.dkr.ecr.us-east-1.amazonaws.com/team/agents/example-app';
const AGENT_YAML = `# a comment\nimage: ${AGENT_REPOSITORY}:toolchain-v3\n`;

function fixture(kind = 'runtime', event = 'pull_request') {
  const identity = {owner: 'example-org', name: 'Example.App', repoID: REPO_ID, ownerID: OWNER_ID};
  const repo = {id: REPO_ID, owner: {id: OWNER_ID}, full_name: 'example-org/Example.App', fork: false, default_branch: BRANCH};
  const path = kind === 'runtime' ? RUNTIME_WORKFLOW : AGENT_WORKFLOW;
  return [identity, repo, {repository: repo, head_repository: repo, status: 'completed', conclusion: 'success',
    head_sha: sha, workflow_id: 5, path, event, head_branch: BRANCH, run_attempt: 1, pull_requests: [{number: 3}]},
  {id: 5, path, state: 'active'}, {number: 3, state: 'open', base: {ref: BRANCH, repo}, head: {repo, sha}},
  [{id: 7, name: `${kind}-${sha}-1`, expired: false, size_in_bytes: 123}], kind,
  {yaml: AGENT_YAML, repository: AGENT_REPOSITORY}];
}

test('same-repo current PR, default-branch runtime, dispatched runtime and agent accepted', () => {
  for (const [args, source] of [[fixture(), 'pull_request'], [fixture('runtime', 'push'), 'main'],
    [fixture('runtime', 'workflow_dispatch'), 'main']]) {
    assert.deepEqual(validate(...args), {artifact_id: '7', sha, kind: 'runtime', source});
  }
  for (const event of ['push', 'workflow_dispatch']) {
    assert.deepEqual(validate(...fixture('agent', event)),
      {artifact_id: '7', sha, kind: 'agent', source: 'main', tag: 'toolchain-v3'});
  }
});

for (const [name, mutate] of Object.entries({
  fork: a => {a[2].head_repository = {...a[1], id: 888, fork: true};},
  reused_name: a => {a[1].id++;},
  wrong_owner: a => {a[1].owner.id++;},
  renamed_but_unconfigured: a => {a[1].full_name = 'example-org/other';},
  other_default_branch: a => {a[1].default_branch = 'other';},
  unset_repository_id: a => {a[0].repoID = NaN;},
  zero_owner_id: a => {a[0].ownerID = 0;},
  failed_run: a => {a[2].conclusion = 'failure';},
  in_progress: a => {a[2].status = 'in_progress';},
  wrong_workflow: a => {a[3].path = '.github/workflows/evil.yml';},
  inactive_workflow: a => {a[3].state = 'disabled_manually';},
  stale_head: a => {a[4].head.sha = 'b'.repeat(40);},
  closed_pr: a => {a[4].state = 'closed';},
  fork_pr: a => {a[4].head.repo = {...a[1], fork: true, id: 888};},
  missing_pr: a => {a[2].pull_requests = [];},
  ambiguous_pr: a => {a[2].pull_requests.push({number: 9});},
  wrong_base: a => {a[4].base.ref = 'other';},
  unsafe_sha: a => {a[2].head_sha = '$(id)';},
  old_attempt: a => {a[2].run_attempt = 2;},
  missing_artifact: a => {a[5] = [];},
  duplicate_artifact: a => {a[5].push({...a[5][0]});},
  expired_artifact: a => {a[5][0].expired = true;},
  huge_artifact: a => {a[5][0].size_in_bytes = 2 ** 32;},
  target_event: a => {a[2].event = 'pull_request_target';},
  wrong_branch: a => {a[2].event = 'push'; a[2].head_branch = 'other';},
  agent_pr: a => {a[6] = 'agent'; a[3].path = a[2].path = AGENT_WORKFLOW;},
  unknown_kind: a => {a[6] = 'constructor';},
})) {
  test(`reject ${name}`, () => {const args = fixture(); mutate(args); assert.throws(() => validate(...args));});
}

test('agent images come only from default-branch runs of the agent workflow', () => {
  const args = fixture('agent', 'push');
  args[2].path = args[3].path = RUNTIME_WORKFLOW;
  assert.throws(() => validate(...args), /workflow identity/);
});

test('an agent image needs a readable declaration naming this repository', () => {
  for (const [name, agent] of Object.entries({
    missing: undefined,
    unread: {repository: AGENT_REPOSITORY},
    no_repository: {yaml: AGENT_YAML},
    other_repository: {yaml: AGENT_YAML, repository: `${AGENT_REPOSITORY}-other`},
  })) {
    const args = fixture('agent', 'push');
    args[7] = agent;
    assert.throws(() => validate(...args), Error, name);
  }
});

test('the declared toolchain tag is read strictly', () => {
  const ok = {
    [`image: ${AGENT_REPOSITORY}:toolchain-v1\n`]: 'toolchain-v1',
    [`# c\n\nimage: "${AGENT_REPOSITORY}:toolchain-v12"  # trailing\r\n`]: 'toolchain-v12',
    [`image: '${AGENT_REPOSITORY}:toolchain-v999999'`]: 'toolchain-v999999',
  };
  for (const [text, tag] of Object.entries(ok)) assert.equal(declaredTag(text, AGENT_REPOSITORY), tag, text);
  for (const text of [
    '',
    '# only a comment\n',
    `image: ${AGENT_REPOSITORY}:latest\n`,
    `image: ${AGENT_REPOSITORY}:toolchain-v0\n`,
    `image: ${AGENT_REPOSITORY}:toolchain-v01\n`,
    `image: ${AGENT_REPOSITORY}:toolchain-v1234567\n`,
    `image: ${AGENT_REPOSITORY}:toolchain-v1@sha256:${'c'.repeat(64)}\n`,
    `image: ${AGENT_REPOSITORY}-evil:toolchain-v1\n`,
    `image: other.example.com/x:toolchain-v1\n`,
    `image: ${AGENT_REPOSITORY}:toolchain-v1\nimage: ${AGENT_REPOSITORY}:toolchain-v2\n`,
    `image: ${AGENT_REPOSITORY}:toolchain-v1\nbuild: .\n`,
    `---\nimage: ${AGENT_REPOSITORY}:toolchain-v1\n`,
    `image: "${AGENT_REPOSITORY}:toolchain-v1'\n`,
    `  image: ${AGENT_REPOSITORY}:toolchain-v1\n`,
    'x'.repeat(64 * 1024 + 1),
  ]) {
    assert.throws(() => declaredTag(text, AGENT_REPOSITORY), Error, JSON.stringify(text.slice(0, 80)));
  }
});

test('repository variables must be canonical numeric IDs', () => {
  assert.equal(parseID('1234567890', 'X'), 1234567890);
  for (const bad of [undefined, '', '0', '01', '-1', '1e9', ' 1', '1 ', '12345678901234567', 'abc']) {
    assert.throws(() => parseID(bad, 'X'), /must be a numeric ID/, String(bad));
  }
});
