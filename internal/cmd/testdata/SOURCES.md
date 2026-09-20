The character cards the commands are run against, written by hand.

| File | What it is |
|---|---|
| `ada.yaml` | the smallest card a run takes: a name and the name of whoever she texts |
| `teasing.yaml` | the same, with a line that names him through a template action, which `persona check` renders |

The configuration files are not here. Each one carries the data directory of
the test that wrote it and the address of the server standing in for the API,
so it is written when the test runs and nowhere else.
