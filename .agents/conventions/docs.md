<!-- agent-scaffold:convention=docs -->
# Documentation conventions

Generic guide installed and refreshed by agent-scaffold; do not edit it here. The project's
own documentation, navigation, and nested `AGENTS.md` files win where they are more specific;
record project-specific facts there, not in this file.

## Find the owner before writing

- Follow the project's documentation map or navigation to the page that owns the fact. Keep
  one owner per detailed contract and link to it from overviews, recipes, and entry points
  instead of repeating it.
- Preserve the existing layout, language, filenames, numbering, and frontmatter. A directory
  name is a clue, not authority; do not restructure while making a content change.
- Change generated output (API references, rendered sites, inventories) through its generator
  or source, never by patching the output.
- Leave content owned by a Wiki, another repository, or a symlinked location with that owner;
  do not copy it in or write through it without authority.
- Add a page only for a distinct owner and link it from the existing navigation; otherwise
  extend the owning page. Do not seed empty directories or template catalogs.

## Current, intended, and historical content

- Distinguish implemented behavior, approved intent, drafts, and history. A plan, an
  approval, or a recent edit is not evidence of implementation; current code and verified
  contracts win.
- Keep dated evidence (release notes, validation records, measurements, external
  comparisons) as history with its date and source revision; do not rewrite it to match the
  present.
- When a plan is completed or superseded, stop it from instructing readers: keep its
  rationale and a pinned historical source, and point to the current owner. Use the project's
  status convention; a small flat status and date are enough where confusion is likely.
- One-off plans and audits normally belong in the issue or PR, not a permanent page, unless
  they record a durable decision.

## Changing documentation

- Update the owning page in the same change as the behavior it describes, and fix readers,
  links, and callers when content moves. Preserve unique content when merging pages.
- Take flags, routes, fields, and schemas from their implementation authority (source,
  `--help`, registrations, parsers), not from other prose.
- Keep entry points such as root READMEs and `AGENTS.md` lean: route to detail rather than
  duplicate it. Keep translated entry points equivalent when the project maintains them.
- Recheck primary sources before changing an external claim, and keep its review date.

## Verification

Run the project's documentation checks. Link and inventory checkers do not prove semantic
accuracy, external links, uncovered anchors, or that examples run; review changed anchors
and commands separately and report which were executed and which were only inspected.
