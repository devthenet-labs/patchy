'use strict';

// The trusted publishers' gate: re-read the triggering run from GitHub's API
// and refuse anything that is not this repository's own successful build of
// an open same-repository PR head or of the default branch. Nothing here
// trusts the workflow_run payload or the artifact's contents.
//
// A run's head_branch is only a name: for a run a tag triggered, or a
// dispatch at a tag, it is the tag's name, so a tag named after the default
// branch carries it too. A default-branch build is therefore proved by
// ancestry: its commit must be the branch's head or an ancestor of it,
// compared by SHA against the head the branches API names.
//
// The repository's identity is its immutable numeric IDs, from the
// repository variables PUBLISH_REPOSITORY_ID and PUBLISH_OWNER_ID. The name
// comes from the run's own context and only selects the API path; a
// repository later created under a reused name fails the ID check.

const BRANCH = 'main';
const SHA = /^[0-9a-f]{40}$/;
const ID = /^[1-9][0-9]{0,15}$/;
// Each image kind is built by exactly one uncredentialed workflow.
const SOURCE_WORKFLOW = {runtime: '.github/workflows/ci.yml', agent: '.github/workflows/agent-image.yml'};
const MAX_MIB = {runtime: 128, agent: 2048};
// The agent image is published as the tag .patchy/agent.yaml declares.
const AGENT_YAML = '.patchy/agent.yaml';
const AGENT_YAML_MAX_BYTES = 64 * 1024;
const TOOLCHAIN_TAG = /^toolchain-v[1-9][0-9]{0,5}$/;

function check(ok, message) {
  if (!ok) throw new Error(message);
}

// Repository variables arrive as strings; anything but a canonical positive
// integer (including an unset variable) fails closed.
function parseID(value, name) {
  check(typeof value === 'string' && ID.test(value) && Number.isSafeInteger(Number(value)), `${name} must be a numeric ID`);
  return Number(value);
}

// The tag .patchy/agent.yaml declares for the agent repository: its one
// non-comment line must be `image: <registry>/<repository>:toolchain-v<N>`,
// naming exactly the repository this publisher writes.
function declaredTag(text, repository) {
  check(typeof text === 'string' && text.length <= AGENT_YAML_MAX_BYTES, `${AGENT_YAML} must be readable`);
  check(typeof repository === 'string' && /^[a-z0-9.-]+(\/[a-z0-9._-]+)+$/.test(repository), 'agent repository');
  const lines = text.split('\n').map(line => line.replace(/\r$/, ''))
    .filter(line => line.trim() !== '' && !/^\s*#/.test(line));
  check(lines.length === 1, `${AGENT_YAML} must hold exactly one image line`);
  const match = /^image:[ \t]+(['"]?)([^'"\s#]+)\1[ \t]*(#.*)?$/.exec(lines[0]);
  check(match, `${AGENT_YAML} must set image`);
  const prefix = `${repository}:`;
  check(match[2].startsWith(prefix), `${AGENT_YAML} must name ${repository}`);
  const tag = match[2].slice(prefix.length);
  check(TOOLCHAIN_TAG.test(tag), `${AGENT_YAML} must name a toolchain-v<N> tag, not ${tag}`);
  return tag;
}

// Whether sha is on the default branch: branch is the branch as the
// branches API names it, with the three-dot comparison from sha to its
// head. sha is the head or an ancestor of it exactly when it is the
// comparison's merge base and the head is not behind it.
function onBranch(branch, sha) {
  if (!branch || branch.name !== BRANCH || !SHA.test(branch.head) || !branch.comparison) return false;
  const c = branch.comparison;
  return ['identical', 'ahead'].includes(c.status) && c.behind_by === 0 &&
    Boolean(c.base_commit) && c.base_commit.sha === sha &&
    Boolean(c.merge_base_commit) && c.merge_base_commit.sha === sha;
}

function validate(identity, repo, run, workflow, pr, artifacts, kind, agent, branch) {
  const {owner, name, repoID, ownerID} = identity;
  check(Number.isSafeInteger(repoID) && repoID > 0 && Number.isSafeInteger(ownerID) && ownerID > 0, 'configured identity');
  check(repo.id === repoID && repo.owner.id === ownerID && repo.full_name === `${owner}/${name}` &&
    !repo.fork && repo.default_branch === BRANCH, 'repository identity');
  check(run.repository.id === repoID && run.head_repository.id === repoID &&
    !run.head_repository.fork, 'forks must never publish');
  check(run.status === 'completed' && run.conclusion === 'success' && SHA.test(run.head_sha), 'successful exact head required');
  check(Number.isSafeInteger(run.run_attempt) && run.run_attempt > 0, 'run attempt');
  check(Object.hasOwn(SOURCE_WORKFLOW, kind), 'image kind');
  const path = SOURCE_WORKFLOW[kind];
  check(workflow.path === path && workflow.state === 'active' && workflow.id === run.workflow_id && run.path === path, 'workflow identity');
  let source;
  if (run.event === 'pull_request') {
    check(kind === 'runtime' && pr && run.pull_requests.length === 1 &&
      run.pull_requests[0].number === pr.number, 'PR association');
    check(pr.state === 'open' && pr.base.ref === BRANCH && pr.base.repo.id === repoID &&
      pr.head.repo && pr.head.repo.id === repoID && !pr.head.repo.fork &&
      pr.head.sha === run.head_sha, 'open same-repository PR at current head required');
    source = 'pull_request';
  } else {
    check(['push', 'workflow_dispatch'].includes(run.event) && run.head_branch === BRANCH, 'default branch only');
    check(onBranch(branch, run.head_sha), `the built commit must be on ${BRANCH}, not only named for it`);
    source = 'main';
  }
  const artifactName = `${kind}-${run.head_sha}-${run.run_attempt}`;
  const matches = artifacts.filter(a => a.name === artifactName && !a.expired);
  check(matches.length === 1, 'exactly one matching artifact required');
  const artifact = matches[0];
  check(Number.isSafeInteger(artifact.id) && artifact.id > 0 && artifact.size_in_bytes > 0 &&
    artifact.size_in_bytes <= MAX_MIB[kind] * 1024 * 1024, 'artifact bounds');
  const result = {artifact_id: String(artifact.id), sha: run.head_sha, kind, source};
  if (kind === 'agent') {
    result.tag = declaredTag(agent && agent.yaml, agent && agent.repository);
  }
  return result;
}

async function guard({github, context, runID, kind, repositoryID, ownerID, agentRepository}) {
  check(/^[1-9][0-9]{0,18}$/.test(String(runID)), 'run ID');
  const identity = {
    owner: context.repo.owner,
    name: context.repo.repo,
    repoID: parseID(repositoryID, 'PUBLISH_REPOSITORY_ID'),
    ownerID: parseID(ownerID, 'PUBLISH_OWNER_ID'),
  };
  const params = {owner: identity.owner, repo: identity.name};
  const {data: repo} = await github.rest.repos.get(params);
  const {data: run} = await github.rest.actions.getWorkflowRun({...params, run_id: runID});
  const {data: workflow} = await github.rest.actions.getWorkflow({...params, workflow_id: run.workflow_id});
  let pr;
  if (run.event === 'pull_request' && run.pull_requests.length === 1) {
    ({data: pr} = await github.rest.pulls.get({...params, pull_number: run.pull_requests[0].number}));
  }
  // A default-branch build's commit, compared with the branch's head by SHA:
  // the branch is named only to the branches API, never as a ref a tag of
  // the same name could shadow.
  let branch;
  if (run.event !== 'pull_request' && SHA.test(run.head_sha)) {
    const {data: b} = await github.rest.repos.getBranch({...params, branch: BRANCH});
    const head = b && b.commit && b.commit.sha;
    check(SHA.test(head), `${BRANCH} head`);
    const {data: comparison} = await github.rest.repos.compareCommitsWithBasehead({...params,
      basehead: `${run.head_sha}...${head}`, per_page: 1});
    branch = {name: b.name, head, comparison};
  }
  // Per-run artifacts only; never search globally or trust metadata from the artifact.
  const artifacts = await github.paginate(github.rest.actions.listWorkflowRunArtifacts,
    {...params, run_id: runID, per_page: 100});
  // The declaration at the built commit, never at a moving ref.
  let agent;
  if (kind === 'agent' && SHA.test(run.head_sha)) {
    const {data: file} = await github.rest.repos.getContent({...params, path: AGENT_YAML, ref: run.head_sha});
    check(file && file.type === 'file' && file.encoding === 'base64' && file.size <= AGENT_YAML_MAX_BYTES,
      `${AGENT_YAML} must be a file`);
    agent = {yaml: Buffer.from(file.content, 'base64').toString('utf8'), repository: agentRepository};
  }
  return validate(identity, repo, run, workflow, pr, artifacts, kind, agent, branch);
}

module.exports = {guard, validate, parseID, declaredTag};
