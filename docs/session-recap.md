# Session recap presentation

The Agent generates recaps after completed turns. The runtime persists them.
`GET /api/v1/sessions` (including search) and `GET /api/v1/sessions/{id}` return
optional `recap` and RFC3339 `recap_updated_at` fields. Metadata PATCH responses
retain these fields. Older sessions and failed first generations omit them.
Generation is asynchronous: a response immediately after a turn may still
contain the previous recap. Reopening or refreshing reads the latest stored text.

`lh session list`, REPL `/sessions`, `/session search` and `/session switch`
display the same text with a diamond marker. There is no client model call,
translation, title replacement, compact-summary fallback or second authoritative
store. Text remains in the conversation's language. No configuration keys change.
