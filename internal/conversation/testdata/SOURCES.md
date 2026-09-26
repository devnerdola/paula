`photo.png` is a copy of `internal/media/testdata/red-blue-8x4.png`, which
`go generate ./internal/media` writes. It is the image a message carries when a
test sends one.

`past.json.gz` is the conversation the live test starts from: 100 days of
texting between the two people of `personas/paula.example.yaml`, written by
`openai/gpt-5-mini` through OpenRouter playing both of them, on the date the
file names. `../gen.go` wrote it, by hand, with the prompts it holds: one answer
plans the days and says who Caio is, and one writes each day, which is asked
again when its times are not those of a day in order. The file holds the plan
and the days as the model wrote them. It is read as it is; a past that must
change is written again.
