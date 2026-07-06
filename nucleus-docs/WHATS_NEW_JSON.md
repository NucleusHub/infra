# What's New — Import from JSON

The **What's New** changelog (Admin Console → *What's New*) can be authored by
hand, or built from a JSON document and imported in one step. This is handy for
generating release notes with an assistant, keeping them in version control, or
copying an announcement between environments.

Import is a **client-side convenience**: the JSON is parsed in the browser and
loaded into the normal draft editor for review. **Nothing is saved until you
press “Save announcement”**, and nothing is shown to users until you **Publish**.
There is no separate import API — it reuses the same create endpoint as the form.

## How to import

1. Admin Console → **What's New**.
2. Click **↥ Import JSON** (next to *+ New announcement*).
3. Either paste the JSON into the box, or click **Load .json file…** to read a
   `.json` file from disk.
4. Click **Load into editor**. The announcement opens in the draft editor.
5. Review the version, per-app tabs, features, and each language. Adjust as
   needed.
6. **Save announcement** to store it as a draft, then **Publish** when ready.

If an app id in the JSON isn't a known registry app (or `platform`), the import
still loads but shows a warning naming the unknown id — fix that tab's app
before saving.

## JSON schema

A single announcement object:

```json
{
  "version": "0.4.0",
  "entries": [
    {
      "app": "orbit",
      "version": "0.2.1",
      "features": [
        {
          "icon": "✨",
          "title": { "en-US": "Faster uploads", "cs-CZ": "Rychlejší nahrávání" },
          "body":  { "en-US": "Large files now upload in parallel.", "cs-CZ": "Velké soubory se nahrávají paralelně." }
        }
      ]
    },
    {
      "app": "platform",
      "version": "0.4.0",
      "features": [
        { "icon": "🎉", "title": { "en-US": "Dark mode everywhere" }, "body": { "en-US": "Every app now respects your theme." } }
      ]
    }
  ]
}
```

### Fields

| Field                    | Type                | Required | Notes                                                                                      |
| ------------------------ | ------------------- | -------- | ------------------------------------------------------------------------------------------ |
| `version`                | string              | ✅       | The **Nucleus platform** version for this release (shown on top), e.g. `"0.4.0"`.          |
| `entries`                | array               | ✅       | One entry per app — each becomes a **tab** in the modal. At least one is required.         |
| `entries[].app`          | string              | ✅       | A registry **app id** (`orbit`, `echo`, `watchlist`, …) or `"platform"` for general notes. |
| `entries[].version`      | string              | –        | That **app's own** version at this release (shown as the tab's group header).              |
| `entries[].features`     | array               | –        | The feature list for that app. An empty list is allowed (the editor adds a blank row).     |
| `features[].title`       | object `{lang:text}`| –        | Heading, keyed by locale tag (`en-US`, `cs-CZ`, …).                                         |
| `features[].body`        | object `{lang:text}`| –        | Description, keyed by locale tag. Optional.                                                |
| `features[].icon`        | string (emoji)      | –        | Optional emoji, max 4 chars.                                                                |

### Rules & normalization

- **Locale keys** should match installed languages (Admin Console →
  *Languages*). Unlisted locales are kept but only surface if that language is
  installed; empty/missing languages fall back to the default language for the
  viewer.
- Unknown fields are ignored. Non-string title/body values are dropped.
- `app` is trimmed; `icon` is truncated to 4 characters. This mirrors the
  server-side sanitization, so **what you preview is what gets stored**.
- The importer is lenient about extra whitespace and field order, but the top
  level **must be a single JSON object** (not an array of announcements).

## Generating JSON with an assistant

A prompt that reliably produces importable output:

> Produce a Nucleus "What's New" announcement as a single JSON object matching
> this schema: `{ "version": string (Nucleus platform version), "entries":
> [{ "app": registry app id or "platform", "version": that app's version,
> "features": [{ "icon": emoji, "title": { "en-US": string, "cs-CZ": string },
> "body": { "en-US": string, "cs-CZ": string } }] }] }`. Use these app ids: hub,
> goal-calendar, watchlist, orbit, echo, anchor, admin, pulse, prism, or
> "platform" for general highlights. Output only the JSON.

Always review the loaded draft — check the versions match the registry and that
every installed language is filled — before publishing.

## Related

- Authoring model and behaviour: see the *What's New* section of the platform
  docs and `core/auth-server/models/WhatsNewAnnouncement.js`.
- Publishing semantics (auto-open once per new publish, per-user seen state):
  `core/auth-server/routes/whatsNew.js` and `core/WhatsNewModal.vue`.
