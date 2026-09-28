# Browser walk-through

Walks the real brand admin and retailer portal (the vendored admin-ui in `ui/dist`) against a freshly started node in headless Chromium:

setup wizard → CSV + images zip import → access request → approval → invitation → retailer sets a password → browse, article page, EAN list, selection zip → API keys (brand and retailer self-service) → product zip, CSV and a whole-catalog zip prepared in the background → automatic upload key → scheduled import from a web address → article page in the admin → Dutch portal.

```sh
e2e/run.sh          # needs Go, Node.js and zip; installs Playwright on first run
```

The script builds node-go, starts it on port 18080 with a temporary data directory (`e2e/tmp`), reads the invitation link from the node's log (`OBX_MAIL_LOG_BODIES=true`) and writes screenshots to `e2e/screenshots`. Every test also fails on any unexpected API error or page error, so mismatches between the node and the UI show up.
