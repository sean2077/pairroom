<!-- pairroom-relay:reference -->
# Files and evidence review

Read this before file transfer, reading a referenced output file, or a review workflow. Prefer small inline replies/deltas.

## Transfer and read

Use `--text-file <path>` or `--text-file -`; do not read and retype it. `--ref <path>` shares path/size/SHA-256 only, no upload: retain the file, verify its hash, read needed sections with authorized access. Changed evidence needs a new publication ID.

`--output-file <new-path>` saves the full envelope: read it before acting. It needs an existing writable parent; never overwrite; retry a fresh path. Paths follow tool cwd; flags require the current CLI. Read full-output files even when stdout shows only a preview; stdout/handed_off is not model acceptance.

## Review

Use `pairroom relay send --review` or `pairroom relay exchange --review`; inspect with `pairroom relay review --id <published-id>`. For another checkout, use `--review-repo <task-checkout>`.

Review independently; pin base/head SHAs and the dirty hash, recheck before closure. Observations are not atomic or approval. Report disagreements and unrun tests; findings need location, evidence and impact. Label hypotheses; retest fixes at the new revision.
