`migrations/` holds the schema changes a later Paula would carry, which the
tests apply through the same runner as the ones under `internal/store/migrations`.

| File | What it is |
|---|---|
| `0001_notes.sql` | a table the schema does not have |
| `0002_notes_colour.sql` | a column added to that table, so the two only work in order |

Running them a second time fails: the table is already there and so is the
column. That is what holds the runner to applying a migration once.
