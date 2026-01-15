<strict_rules>

- do not add any comments to the generated code. it's not productive.
- fix all diagnostic issues before returning.
- do not swallow any error. handle all errors.
  </strict_rules>

<test_environment>

</test_environment>

<engineering>
- Call out conflicts between instructions (if any)
- During bug analysis, focus more on root causes.
- Aim for functionality & simplicity.
</engineering>

<project_description>
mono is a backend for conductor.build, that easily allows developers to create parallel and isolated dev environments for each conductor workspace. It enables running multiple concurrent workspaces without port conflicts or resource contention.
it works by inserting itself into 3 hooks, provided by conductor. (ie. setup, run, archive) and allowing users to configure their dev environments using mono primitives.
an important feature in mono is ISOLATION, where operations in one enviroment should not affect operations in others. To speed up repeated builds, we provide inbuilt caching functionality for each workspace.
a workspace is a git worktree, and mono helps turn that worktree into an isolated dev environment. see docs/design/graduated/mono.md for more information and docs/design/graduated/caching.md for the caching architecture.


</project_description>