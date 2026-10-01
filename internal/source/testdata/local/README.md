# sard local fixture

Committed fixture tree for the local directory source tests (docs/PLAN.md
§10 T4, §12). It is deliberately mode-independent: it holds no Markdown
file inside a noise dir and no untracked or gitignored Markdown, so
`git ls-files` (when this tree sits inside the sardonyx repo) and a
filesystem walk discover the same set of files.

Two default noise dirs are intentionally absent here: `.git` (a `.git`
entry would make the tree look like a repository of its own) and `.idea`
(the sardonyx repo's .gitignore ignores it, so it cannot be committed).
Their exclusions are covered by the temp-dir walk tests in
local_test.go (TestLocalWalk, TestLocalFakeGitFallback).
