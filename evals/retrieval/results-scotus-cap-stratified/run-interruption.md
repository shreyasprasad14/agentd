# Run interruption

The run described in run-metadata.json (started 2026-09-16T20:10:58Z) finished the five
category files (paraphrase, citation, case_name, term_of_art, mixed). It was then interrupted
partway through stratified-all when the laptop lost power: the last model request was about
2026-09-16 18:37 EDT, and the machine was back on AC at 19:30 EDT. The stalled eval process was
stopped and wrote no output; the eval command only writes results when a file finishes.

Resumed 2026-09-16T23:36:21Z from stratified-all, under caffeinate. Before resuming, the
sha256 of every file in ranker_files_sha256 matched run-metadata.json, and corpus_lexical_stats
still read chunks=70736 avg_length=118.365, so both halves of the run measured the same ranker.
