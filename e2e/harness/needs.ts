// Builders for the GraphQL answer of the needs-you read (bazel.setNeeds).
export const ME = 'e2e-user';
export const at = (h: number) => new Date(Date.UTC(2026, 10, 1) + h * 3600_000).toISOString();

/** A PR as the GraphQL search returns it, with nothing to say. */
export function node(repo: string, number: number, title: string, author: string, extra: Record<string, unknown> = {}) {
  return {
    number, title, url: `https://github.com/${repo}/pull/${number}`, isDraft: false,
    createdAt: at(0), updatedAt: at(50), additions: 1, deletions: 0, changedFiles: 1,
    headRefName: 'b', baseRefName: 'main', headRefOid: 'x', reviewDecision: '', mergeable: 'MERGEABLE',
    author: { login: author }, repository: { nameWithOwner: repo },
    commits: { nodes: [{ commit: { committedDate: at(1), statusCheckRollup: null } }] },
    forcePushes: { nodes: [] }, requests: { nodes: [] }, reviews: { nodes: [] },
    comments: { nodes: [] }, reviewThreads: { nodes: [] },
    ...extra,
  };
}
export const review = (login: string, state: string, h: number) => ({ id: `${login}-${h}`, author: { login }, state, submittedAt: at(h) });
export const pushed = (h: number, state: string | null = null) => ({ nodes: [{ commit: { committedDate: at(h), statusCheckRollup: state && { state } } }] });
/** A PR whose review is requested from ME. */
export const asked = (repo: string, n: number, title: string, author = 'alice', h = 5) =>
  node(repo, n, title, author, { requests: { nodes: [{ createdAt: at(h), requestedReviewer: { __typename: 'User', login: ME } }] } });
