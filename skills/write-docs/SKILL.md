---
name: write-docs
description: 'Write or edit prose documentation so it does not read as machine output. Covers READMEs, runbooks, guides, changelogs, design docs, PR bodies, comment blocks and release notes. Bans the em dash, ~90 tell-tale words and the stock rhetorical shapes; strips context that only the requester has (leftovers of the prompt, diffs written as state, personal hostnames and paths); forces varied sentence length, concrete numbers, named tradeoffs and maximum information per word (frames deleted, nominalizations turned back into verbs, padding cut); caps scope: rationale gets a clause not a section, no X-not-Y frames, no aphorism capstones, no pitch intros. Also shapes the document for a skimming reader who wants to act: command before prose, numbered procedures, real durations, a stated way to check it worked, headings that carry the story, and a vhs demo GIF in a GitHub tool README. Composes with i-have-adhd, which does the same for replies. Load BEFORE writing the first line of any doc, and whenever asked to "make this sound human", "de-LLM this", "rewrite this doc" or "no em dashes".'
license: MIT
metadata:
  tags: "writing, documentation, style, prose, editing, concision"
  category: "writing"
---

# write-docs

Machine prose is not bad because it is wrong. It is bad because it is _uniform_.
Every sentence lands at the same length, every paragraph opens with a frame
before the fact, every claim is hedged to the same temperature, and the whole
thing ends by telling you what it just told you.

A human writing at speed does none of that. They drop a four-word sentence. They
name the file. They say the thing that breaks.

Sections 1 to 6 are about the sentences. Section 7 is about the document, and
whether a reader who landed in the middle of it can act. Section 10 is about
how much document to write at all, which is where this style fails hardest.
All of it has to hold. Prose that reads human but strands the reader has failed
at the only thing a doc is for.

## 1. The em dash, and what replaces it

Ban: `—` (em dash), `–` (en dash as a connector), and `-` (spaced hyphen,
which is the same move wearing a hat). Ordinary hyphens in compounds are fine:
`keyboard-first`, `hot-reload`, `3-second`.

Four fixes, in the order to try them:

| The dash was doing                   | Use instead                                          |
| ------------------------------------ | ---------------------------------------------------- |
| Introducing a list or an explanation | A colon                                              |
| Wrapping an aside                    | A comma pair, or parentheses                         |
| An abrupt turn                       | A full stop, then the turn                           |
| Joining two loose thoughts           | Two sentences. Usually the second one was the point. |

Bad: `The theme system has two axes — colour and form — and they never mix.`
Good: `The theme system has two axes: colour and form. They never mix.`

Bad: `It runs from hypridle — so a venv Python must not be picked up.`
Good: `It runs from hypridle, so a venv Python must not be picked up.`

Bad: `hyprctl reload returns ok on a broken config — hyprctl configerrors is the real check.`
Good: `hyprctl reload returns ok on a broken config. hyprctl configerrors is the real check.`

If a sentence needs three dashes to hold together, it is two sentences pretending
to be one. Split it.

Priority, when editing existing text: the pair of dashes wrapping a mid-sentence
aside (`— the part that actually matters —`) dies first. In an observed human
edit pass that pair was cut on sight while a lone joining dash survived. The ban
covers both. The pair is the one readers flinch at.

## 2. Words that mark the text as generated

Never, in prose:

```text
delve, leverage (as a verb), utilize, seamless, robust, streamline, harness
(as a verb), empower, elevate, unlock, foster, underscore, showcase, embark,
myriad, plethora, tapestry, testament, realm, landscape (figurative),
navigate (figurative), journey (figurative), game-changer, cutting-edge,
state-of-the-art, best-in-class, holistic, synergy, paradigm, bespoke,
curated, transformative, revolutionize, unparalleled, supercharge,
meticulous, intricate, nuanced, boasts, dive into, deep dive
```

Phrases, same rule:

```text
it is worth noting, it is important to note, keep in mind that, that said,
that being said, when it comes to, at its core, at the end of the day,
in today's world, the world of, let's dive in, let's explore, in conclusion,
to sum up, needless to say, rest assured, look no further, we have you
covered, a wide range of, a variety of, plays a key role, serves as,
acts as, first and foremost, whether you are X or Y
```

**The exception matters as much as the rule.** Some of these are the correct
word. A `comprehensive` test suite that genuinely covers every branch is
comprehensive. A step that is `crucial` because skipping it bricks the boot is
crucial. Keep the word when it carries a fact. Cut it when it is only warmth.

Over-avoidance has its own smell. Writing "make certain that" to dodge "ensure"
is worse than "ensure". The fix is almost always to delete the clause, not to
find a synonym for it.

## 3. Write for a reader who was never in the room

The most common leak is not a word. It is context that exists only in the
conversation that produced the doc. You know Homebrew was removed, so the doc
says "and no Homebrew packages". The reader never knew it was there, and now has
a question they did not need.

Assume the reader has never spoken to you, never saw an earlier version of the
project, and does not know what was asked for. Every sentence has to survive
that.

Four kinds of leak:

**Residue of the request.** `As requested`, `as you mentioned`,
`per your instruction`, `the changes you asked for`, `hope this helps`.
Delete on sight.

**Residue of the conversation.** `We decided to use TOML`,
`the approach we settled on`, `as discussed above` pointing at nothing.
If the decision matters,
write the reason instead of the meeting: `TOML, because tomllib is stdlib and
PyYAML would be a new dependency.`

**A diff written as if it were state.** `No longer uses X`, `the new script`,
`this has been updated`, `instead of the old approach`, `recently added`. New
relative to what? The reader has exactly one version, this one. Describe what
is, not what moved.

**Someone else's specifics.** A hostname, a GPU model, a home directory, a
monitor connector, a personal absolute path. Use the role or a placeholder.

### Absence, when it earns its place

Sometimes what is missing is the useful fact. The test is whether the reader
needs it to act. If they do, state the rule, not the removal.

Bad: `The setup script no longer installs Homebrew packages.`
Good: `Every tool comes from pacman.`

The second survives because a reader arriving with brew muscle memory would
otherwise go looking for it. The first only parses for someone who watched it
change.

Same test elsewhere. `Deliberately not managed: the SSH config, because it names
internal hosts` is something the reader can act on. `We took the SSH config out`
is a diary entry.

### Where the diff does belong

Changelogs, migration guides and design records exist to describe change. They
get the past tense and the comparison, because the before-state is the subject.
Everything else, README and runbook and reference and guide, describes the
system standing still. Know which one you are writing. Do not let a changelog
sentence wander into a README.

## 4. Shapes to avoid

- **The rule of three when two things exist.** "Fast, simple and reliable"
  usually means `fast`. Say which one.
- **The `not just X, it's Y` swap.** Also `not merely`, `more than just`.
  Delete the first half.
- **The `X, not Y` frame, in every spelling.** `real data, not guesswork`,
  `measured, not assumed`, `rendered, not shown as source`. The comma is not
  the tell, the contrast is: `instead of`, `rather than`, `as opposed to`,
  `in place of` and `X, but not Y` are the same move and get cut the same way.
  Nobody proposed Y. State X and stop.

  Watch this one when writing a doc about a system that has a wrong answer to
  avoid. Knowing what the code guards against pulls the guard into the prose.
  Observed, after this rule was already written down: `the brief says so
  instead of reporting that nothing depends on the package` was cut by its
  reader to `the analysis summary will state this is the case`. The second
  half was the failure mode I had just spent an afternoon fixing, which is
  exactly why it felt like content and was not.

  Keep a negation only when the reader would otherwise believe the wrong
  thing: `AUR packages are not rebuilt for you` survives because they would.
- **The aphorism capstone.** A fact, then its moral: `it is an exact diff of
  two lists, and an exact answer beats a confident guess`. The maxim is
  applause for the sentence before it. End on the fact.
- **Rhetorical question as a heading.** `## So what does this mean?` No. Write
  the heading as the answer.
- **Bold lead-in on every bullet.** One or two is a table trying to escape. Make
  it a table.
- **Emoji in headings.** No 🚀, no ✨, no 🎯.
- **The Overview section that overviews nothing.** If the first paragraph could
  be deleted with no loss, it should be.
- **The Conclusion that restates the doc.** Docs end when the last fact ends,
  or on a next step (§7). What is banned is the recap, not the ending.
- **Uniform section length.** Real docs are lopsided, because real topics are.
  A section with one fact gets one line.
- **A table for two facts.** That is a sentence.
- **A bulleted list of things that are not a set.** Bullets are for peers. If
  the items depend on each other, that is a paragraph or a numbered procedure.

## 5. What to do instead

**Vary sentence length on purpose.** In any paragraph over three sentences, one
should be under seven words and one should be over twenty. If every sentence is
twelve to eighteen words, the paragraph reads as generated no matter which words
are in it. This is the single highest-signal fix on this page.

**Lead with the fact.** Cut any opening clause that announces what the sentence
is about to do.

Bad: `It is important to understand that the config is rendered, not edited.`
Good: `The config is rendered. Editing it directly loses the change on next switch.`

**Attach a number, a path or a command to every claim.** "Fast" is nothing.
"12ms, down from 400ms" is something. "Configurable" is nothing.
"`~/.config/monitors.toml`, one table per role" is something.

**Name the cost.** Generated prose describes only the happy path. Human docs say
what breaks, and how you will know.

Good: `A dead hypridle means the machine never locks, with no visible symptom.`

**Have an opinion and mark it as one.** "Use X" beats "you may wish to consider
X". If it is a judgment call, say so in the same breath: "Prefer X. Y works, but
it needs a second daemon."

**Use contractions where you would say them out loud**, and not where you
would not. Applying them uniformly in either direction is its own tell.

**Fragments are allowed.** Used sparingly. Like that.

**Say "you" to the reader and "I" or "we" only when someone actually decided
something.** `We chose TOML because tomllib is stdlib` is a real sentence.
`We believe in simplicity` is not.

## 6. Say it in fewer words

Concision is a ratio, not a word count. Facts per word. Adding a fact the reader
needs makes a document denser; cutting one makes it shorter and worse. Never
trade information for length. Trade away the words wrapped around it.

Seven cuts, in the order that pays.

**Delete the frame.** Any clause announcing what the sentence is about to do.
`It is important to understand that the config is rendered` becomes `The config
is rendered.`

**Turn nominalizations back into verbs.** A nominalization is a verb wearing a
noun's clothes: `perform a validation of` for `validate`, `make a decision` for
`decide`, `provides support for` for `supports`. Each one drags an empty verb
and an article behind it. This is the biggest single source of bloat in
technical prose.

**Name the thing.** `the configuration file` becomes `themes.toml`. Shorter and
more informative at the same time, which is the rare cut that costs nothing.

**Cut degree adverbs.** `very`, `quite`, `really`, `extremely`, `incredibly`,
`particularly`. They add length and subtract precision. If the degree matters it
is a number.

**Kill stock padding.** `in order to` is `to`. `due to the fact that` is
`because`. `at this point in time` is `now`. `a number of` is the number.
`there are three files that control this` is `three files control this`.

**Active voice where the actor matters.** `The config is read by the script`
becomes `The script reads the config`. Two words shorter, and it names who acts.

**One idea per sentence.** Stacked `which` and `that` clauses are usually two
sentences that got glued together.

### The test, and the limit

Draft the paragraph. Rewrite it in half the words. Keep whichever version still
holds every fact, which is usually the short one, and usually better than either
original.

Stop when a cut removes a fact, or when the result needs a second read.
Compression past comprehension is not concision, it is a different failure.
`Config renders. Edit template.` is shorter than the sentence it replaced and
carries less. Density is measured at the reader, not in the file.

§5 asks for varied sentence length and this section asks for fewer words. They
do not fight. Variance is rhythm, density is waste. A thirty-word sentence is
fine when all thirty words work.

## 7. Shape it for someone skimming on their way to doing something

Almost nobody reads a document. They arrive from a search or a link, land in the
middle, and want one thing. The rules below are lifted from the `i-have-adhd`
skill, which shapes chat replies for a reader with small working memory and high
friction between knowing and doing. Documents have that reader too. The two
compose cleanly: that skill governs replies, this one governs files.

Five facts drive all of it.

1. Working memory is small. Anything not on screen is gone.
2. Knowing the answer is not doing it. The gap between the two is where readers quit.
3. Starting is the hardest step. The first action has to be small and copy-pasteable.
4. Time estimates feel uniform. "Some setup" and "an afternoon" land identically.
5. Visible progress matters. A reader who cannot tell whether it worked assumes it did not.

**Put the command before the prose.** A section about installing opens with the
command block. The explanation goes underneath, for whoever wants it. Nobody
should have to read a paragraph to reach the line they were always going to
copy.

**Numbered steps for procedures, bullets for sets, prose for everything else.**
One bounded action per step. A step containing "and then" twice is two steps.
This is the line §4 was drawing: bullets are for peers, numbers are for order,
and when the items depend on each other in a way neither shape captures, write
the paragraph.

**Say how long it takes.** Vague duration is the same as no duration. `Roughly
20 minutes on a fresh box, most of it the AUR build` is usable. `Takes a while`
is not.

**Say what success looks like.** This is the most commonly missing thing in
technical writing. A procedure that stops at the last command leaves the reader
with no way to know it worked, and a silent failure reads exactly like a
success. Name the check.

Bad: the last step is `systemctl --user enable --now hypridle.service`
Good: `systemctl --user status hypridle` should print `active (running)`. A dead
hypridle means the machine never locks, with no other symptom.

**No reference that runs on memory.** `as mentioned above`, `recall that`, `the
approach from the previous section`. Someone who landed here from a search never
saw it. Repeat the fact in one line, or link to the heading. This is the same
leak as §3, pointed inward at your own document instead of at the conversation.

**Cap prose lists at five.** Past five, split into must and optional, or now and
later. Five ranked beats ten flat. Reference material is exempt: a flag table or
a package manifest is a lookup, not a list anyone reads top to bottom.

**Errors get symptom, cause, fix, in that order, with no drama.** No `Oops`, no
`Something went wrong`, no `Unfortunately`.

Bad: `Unfortunately, the config may sometimes fail to load properly.`
Good: `hyprctl reload returns ok on a broken config. hyprctl configerrors is the
real check.`

**End on a fact or a next step, never a summary.** §4 bans the conclusion that
restates the document. It does not ban having an ending. A section that leaves
work open names one thing to do next, small enough to do immediately.

**Headings are the interface.** Most readers read nothing else. Each one states
what is in the section rather than gesturing at it. `Theming` beats `About the
theme system`. `Fix a display that will not wake` beats `Troubleshooting`.

## 8. Worked examples

Bad:

## Overview

>

The theme system is a robust and comprehensive solution that empowers users to
seamlessly customize their desktop experience. Whether you're a power user or
a newcomer, the system provides a wide range of options — from colours to
corner radii — allowing you to craft a truly bespoke environment. It's worth
noting that all configuration is centralized, making it easy to get started.

Six sentences of temperature and no facts. What file? How many themes? What
happens on a bad value?

Good:

## Theming

>

Two axes, kept separate. A **desktop** theme is colour only: palette, wallpaper
folder, typeface. A **material** is form only: corners, gaps, blur, opacity.
Neither leaks into the other.

>

`themes.toml` is the registry. It lists the active pair and every theme
registered for the bar menu. Registration is explicit rather than a directory
scan, so a half-finished palette does not show up in the switcher.

>

Edit the templates, never the generated output. A render replaces stale files
in place.

Same subject, a third longer, and every sentence carries something you could act
on or be wrong about.

### A procedure, for §7

Bad:

> ## Getting Started
>
> Setting up the environment is a straightforward process. First, you will want
> to run the setup script, which will handle installing all of the necessary
> packages and creating the required symlinks. Once that has completed, you can
> enable the services. It's worth noting that this may take some time depending
> on your system.

No command you can copy without reading for it, no duration, and nothing that
tells you whether it worked.

Good:

> ## Install
>
> ```bash
> bash setup.sh
> ```
>
> Roughly 20 minutes on a fresh box. The AUR build is most of it.
>
> 1. Run `bash setup.sh`. It bootstraps `paru` if absent.
> 2. Reload the user units: `systemctl --user daemon-reload`
> 3. Enable the idle daemon: `systemctl --user enable --now hypridle.service`
>
> Check it took: `systemctl --user status hypridle` prints `active (running)`.
> A dead hypridle means the machine never locks, and there is no other symptom.
>
> Next: pick a theme with `SUPER+T`.

Command first, three bounded steps, a real number, a verification, one small
next action.

## 9. Revision pass

Run this over the draft before handing it over. Two passes, both separate from
writing. Do not try to do either while drafting.

**Pass one, the prose.**

1. `grep` the draft for `—`, `–` and `-`. Fix every hit with §1.
2. Reread as a stranger. Flag every sentence that needs the conversation, an
   earlier version, or your machine to parse. Rewrite per §3 or cut it.
3. Read the first sentence of each section. Delete any that only announces the
   section.
4. Read the last paragraph. Delete it if it restates. It usually does.
5. Find the longest paragraph. Check for a sub-seven-word sentence in it. Add
   one if it is missing.
6. Scan for the §2 lists. For each hit, ask whether it carries a fact. Delete or
   restructure; do not swap in a synonym.
7. Count the claims with no number, path, command or version attached. Each one
   is either a fact you have not looked up or a sentence you do not need.
8. Check every bulleted list. If the items are not peers, it is prose.
9. Take the longest paragraph and rewrite it in half the words. Keep the short
   version unless it dropped a fact (§6).
10. Grep for `in order to`, `due to the fact`, `there are`, `very`, `perform a`.
    Each is a shorter sentence waiting.
11. Every `the X file` or `the script` that has a real name: use the name.
12. Grep prose for `, not `, `instead of`, `rather than`, `as opposed to`. Keep
    a hit only when the reader would believe the wrong half without it. The
    rest is rhetoric (§4), and the synonyms hide from a grep for the comma
    form alone.
13. Read the last sentence of each paragraph. If it generalizes the sentence
    before it, cut it (§4).

**Pass two, the shape.**

14. Read only the headings, in order. Do they tell the story on their own? If a
    heading gestures instead of stating, rewrite it.
15. Find every procedure. Does it end with a way to check it worked? Add one.
16. Anything longer than a single step needs a duration in real units.
17. Search for backward references (`as above`, `recall that`). Repeat the fact
    or link to the heading.
18. Check each section opens with the thing to do, not the reason for it.
19. Count prose lists. Over five items, split into must and optional, or now and
    later. Tables and manifests are exempt.
20. For every section, name what the reader can no longer do if it is deleted.
    No answer means the section goes, not just its padding (§10).

In a MARSHAL run, do steps 1, 2 and 6 with Grep over the file: the dash
characters for step 1, home-directory paths and host names for step 2, and
each word and phrase in the §2 lists, case-insensitive, for step 6. Ignore
hits inside fenced blocks and inline code, and hits that quote a tell
rather than commit it. Grep catches vocabulary, not shape. Steps 3, 4, 5, 7
and 8 are yours.

## 10. Scope: how much of the doc to write

Everything above fixes sentences. The larger failure is writing sections a
human would not have written at all. The evidence is a real edit: a README
written in this skill's style ran 275 lines, and its owner cut it to 150.
Nearly everything cut was justification. A motivation section, an incident
story behind a feature, a benchmark table defending a default, a `known_hosts`
analogy, a rationale clause welded to every hardening decision. What survived
was behavior.

**Describe behavior. Ration the why.** Rationale earns a clause when it changes
what the reader does, and nothing when it does not. One why per feature is the
ceiling, not the target. When every paragraph argues, the doc reads as a thesis
defense, and the reader only wanted to drive the tool.

Bad: `` `sudo`, `pacman` and `makepkg` are denied: the tool that answers
"should I install this?" must not be able to install it. ``
Good: `` `sudo`, `pacman` and `makepkg` are denied. ``

The same cut works at section scale. `Pinned to claude-sonnet-5.` replaced a
benchmark table and two paragraphs proving the pin was measured. A
three-sentence paragraph on cache invalidation kept only its first line, the
one naming the cache key. If the measurement or the history matters, it lives
in a design doc or a commit message, and the README trusts the reader.

**The intro is one flat sentence.** Category plus job: `A TUI tool that helps
you determine whether to install a package update.` That replaced a
two-paragraph pitch built on a bolded rhetorical question. Hooks are for
landing pages.

**A tool's README on GitHub gets a demo GIF.** When the README belongs to a
tool you built and the repo is going on GitHub, record one with
[vhs](https://github.com/charmbracelet/vhs) and put it directly under the intro
sentence. Commit the `.tape` next to the GIF so it can be re-recorded when the
output changes:

```sh
vhs new docs/demo.tape    # edit: Output docs/demo.gif, then the commands to type
vhs docs/demo.tape        # writes docs/demo.gif
```

```markdown
![demo](docs/demo.gif)
```

Record the real command on real input, short enough to loop in under 20
seconds. Nothing in the recording may show a token, a home path or a hostname
(§3). Check it worked: open the GIF and watch it to the end before you commit.
Libraries, internal repos and READMEs that are not about a tool skip this.

**War stories and analogies go elsewhere.** The incident that motivated a
feature, the comparison to `known_hosts`: each is a paragraph the reader must
read to find out they did not need it.

**Mundane verbs.** `hands Claude Code the package identity` came back as
`launches a Claude Code process and gives it the package identity`. The vivid
verb is the writer showing through. The literal one disappears into the doc.

**Punch is a uniformity too.** §5 varies sentence length. Vary temperature the
same way. When every sentence lands clipped and quotable, that evenness reads
as generated exactly like the twelve-to-eighteen-word drone it replaced. Most
sentences should be plain second-person statement, tool as actor, a little
slack tolerated. Spend the pressure on the one fact that breaks systems.

**The section test.** For each section, name what the reader can no longer do
without it. "It adds colour" is not an answer. A badge legend the interface
already shows, the tour of every supported backend, the design philosophy:
delete whole, do not trim.

**Align table pipes in the source.** The same edit re-padded every table so the
columns line up raw. Cheap, and it survives being read as plain text. Do it
from the start.

Measure the padding against the rows that are there. Deleting three rows from a
five-row table leaves every column sized for the widest deleted cell, which
reads as aligned until someone looks at the right-hand pipe. Observed: two rows
survived a cut and both were re-padded by hand afterwards. Re-measure whenever
rows are added or removed, not only when the table is written.

**Name the artifact the reader meets, not the one in the code.** A pipeline has
an input and an output and the source names both; the reader only ever sees
one. `the brief says so` was corrected to `the analysis summary will state
this`. The brief is the prompt going in, and nobody reading a README has one.
Same test as §3: this is your own vocabulary leaking, from the code instead of
from the conversation.

## 11. When this does not apply

- **Quoting a source.** Never edit someone else's words to fit this page.
- **Code, identifiers, output, filenames.** Off limits. A CLI flag named
  `--deep-dive` keeps its name.
- **An existing doc with a settled voice.** Match the file you are editing. A
  patch that switches register mid-document is worse than either style alone.
  Rewrite the whole file or match it. Ask which.
- **Line width in Markdown prose.** Wrap what you author at the width the file
  already uses, and leave paragraphs you did not touch alone. Reflowing them
  buries a one-sentence change in a twenty-line diff. Observed twice on the
  same file: replacement sentences were written as single long lines, dropped
  into wrapped paragraphs, with no reflow either side and no complaint. Source
  width is not what the rendered page looks like, and it is not worth a diff.
  Code comments are the opposite case: those wrap.
- **Formats with a fixed shape**, such as conventional commits, RFCs or a
  changelog schema. The shape wins. Apply the vocabulary and sentence rules
  inside it.
- **Chat replies.** This is for documents. Response style is set elsewhere, and
  `i-have-adhd` is the skill for it.
- **A rule that would delete the content.** When a rule fights the thing the doc
  exists to say, the content wins and the shape stays. A security note that
  needs six items gets six. A reference page is mostly tables and that is
  correct. Bend the rule, do not drop the fact.
