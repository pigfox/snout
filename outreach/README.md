# outreach

A daily batch of one-to-one cold emails from `hello@trireens.com` to the
largest US SEO agencies. It uses only the standard library and is a separate
module, so it is not part of `github.com/pigfox/snout`.

## Files

- `agencies.csv` lists 49 large US SEO and search agencies, roughly biggest
  first by headcount. Each row has the agency's website and its HQ where known.
  The `email` column starts empty and `-find` fills it in.
- `template.txt` is the message. The first line is `Subject:`, then a blank
  line, then the body. `{{.Agency}}`, `{{.Website}}`, `{{.City}}` and
  `{{.State}}` are filled in for each agency. **Write your pitch in it before
  sending.**
- `sent.csv` is created on the first send. It logs every address written to,
  so no agency is emailed twice. Back it up, because deleting it means repeats.
- `suppress.txt` is created by you. It lists addresses (`x@agency.com`) or
  whole domains (`@agency.com`), one per line, that are never written to. Add
  every unsubscribe reply and every bounce here.

## Once: find the addresses

```
go run . -find
```

This fetches each agency's homepage and its contact, about and privacy pages,
then writes the best published address on the agency's own domain into
`email`, preferring hello@, info@, contact@ or sales@. It records the page the
address came from in `email_source`. It never guesses an address, so an agency
that offers only a contact form keeps an empty `email` and is skipped. Look over
the results before the first send.

## Every day: send

```
export SMTP_HOST=smtp.gmail.com SMTP_PORT=587          # Google Workspace, or your provider
export SMTP_USER=hello@trireens.com SMTP_PASS=<app password>
export OUTREACH_POSTAL_ADDRESS="Trireens, <street>, <city, state zip>"

go run .                 # dry run: prints today's messages, sends nothing
go run . -send           # sends up to 20 (-limit), 45s apart (-gap)
```

To run it every weekday morning, add a cron entry like this:

```
30 9 * * 1-5  cd /path/to/snout/outreach && . ./.env && go run . -send >> send.log 2>&1
```

## Rules this follows, and you must too

US B2B cold email is legal under CAN-SPAM if every message meets these rules:

- It has an honest From and Subject.
- It includes your physical postal address. The tool refuses to send without it.
- It gives a working way to opt out. The tool adds an opt-out line and a
  `List-Unsubscribe` header.
- You honour opt-outs within 10 days. To do that, add them to `suppress.txt`.

Deliverability: set up SPF, DKIM and DMARC for trireens.com before the first
send, and keep volume low (20 a day is the default). Reply to responses
personally.
