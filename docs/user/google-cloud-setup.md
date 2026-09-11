# Getting Google Drive API credentials

Drivel does **not** ship with a shared Google application identity, and there is
no hosted Drivel service. To sync with Drive you create your own **OAuth client
ID and secret** in a Google Cloud project you own; Drivel uses them to talk to
your Drive on your behalf. You own these credentials: treat them as secrets, keep
them out of version control (they are gitignored), and revoke them from the Cloud
Console if they ever leak.

This is a one-time setup, about five minutes, and free — the Drive API's
per-project quotas are generous for personal use. What you produce here is a
`credentials.json` file (or a client ID + secret you paste into `drivel login`);
`drivel login` then exchanges it for a `token.json` scoped to your account.

> **Terminology.** These are *OAuth client credentials* (a client ID + secret),
> not a Google "API key." The Drive API rejects plain API keys for user-data
> access — it requires OAuth. So there is no API-key field to copy; you create an
> **OAuth client ID** of type **Desktop app**.

---

## Step 1 — Create or select a Google Cloud project

1. Sign in at the [Google Cloud Console](https://console.cloud.google.com/) with
   the Google account whose Drive you want to sync (or a separate "developer"
   account — the project owner and the synced account need not be the same).
2. Open the **project picker** in the top bar and click **New Project**.
3. Give it any name (for example `drivel`). A billing account is **not** required
   for the Drive API. Click **Create**.
4. Make sure the new project is the **selected** project in the top bar before
   continuing — every step below is scoped to the active project.

## Step 2 — Enable the Google Drive API

1. Go to **APIs & Services → Library**
   ([direct link](https://console.cloud.google.com/apis/library)).
2. Search for **Google Drive API** and open it.
3. Click **Enable**. (If it already says "Manage," it's enabled.)

Without this, token exchange may succeed but every Drive call returns
`403 accessNotConfigured`.

## Step 3 — Configure the OAuth consent screen

Google requires a consent screen before it will issue any OAuth client. Under
**APIs & Services → OAuth consent screen**
([direct link](https://console.cloud.google.com/apis/credentials/consent)):

1. **User type:**
   - **External** — for a personal `@gmail.com` account. This is the normal
     choice. It does *not* mean "published to the world"; you can keep the app in
     **Testing** status and restrict it to test users you list (below).
   - **Internal** — only available if your account is part of a Google Workspace
     organization. It skips the test-user and verification friction entirely, so
     prefer it if you have it.

2. **App information:** fill in an **app name** (anything, e.g. `drivel`) and pick
   your own email for the **user support email** and, later, the **developer
   contact** email. You do **not** need a homepage, an application privacy policy,
   or an authorized domain — leave those blank. Nobody but you uses this app.

3. **Scopes:** you can **skip** adding scopes on this screen. Drivel requests the
   scope it needs at login time (`drive`, `drive.readonly`, or `drive.file`);
   pre-declaring them here is optional and only affects the consent-screen text.

4. **Test users:** add the Google account(s) whose Drive you will sync. Only
   listed test users can complete login while the app is in Testing status.

5. Leave the app in **Testing** status. You do **not** need to "Publish" it or
   submit it for Google verification for personal use.

## Step 4 — Create the OAuth client ID (Desktop app)

Under **APIs & Services → Credentials**
([direct link](https://console.cloud.google.com/apis/credentials)):

1. Click **Create Credentials → OAuth client ID**.
2. **Application type:** choose **Desktop app**. This is important — Drivel uses
   the installed-app / loopback redirect flow, which the Desktop app type enables.
   (Do **not** pick "Web application"; its redirect-URI rules differ.)
3. Name it (e.g. `drivel-desktop`) and click **Create**.
4. In the dialog, copy the **Client ID** and **Client secret**, or click
   **Download JSON** to save the client-secret file.

You now have everything Drivel needs.

## Step 5 — Feed the credentials to Drivel

```sh
drivel login -account personal
```

`-account NAME` scopes the login: credentials and token are written under
`~/.config/drivel/NAME/`, and an `[account.NAME]` block is appended to
`~/.config/drivel/config.toml`. Without it, the wizard writes `credentials.json`
and `token.json` into the working directory instead — fine for a single mount
driven by flags.

Provide the client ID and secret either way:

- **Interactively** — paste them when prompted, or put the downloaded JSON at
  `./credentials.json` and press Enter to accept what the wizard finds in it.
- **Non-interactively** — `drivel login -client-id ID -client-secret SECRET
  -scope drive`.

The wizard then prints an authorization URL and captures the result: a loopback
server on port **53682** catches the browser redirect automatically, and if the
browser cannot reach that address you paste the redirect URL — or just the
`code=` value — into the prompt. On success it saves the token and prints the
account you signed in as.

Then mount:

```sh
drivel mount                     # everything in the config file
```

or, with flags only:

```sh
drivel mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json
```

---

## Choosing a scope

`drivel login -scope` (or the interactive menu) picks how much access the token
grants:

| Scope            | Access                                                     | Use when |
|------------------|------------------------------------------------------------|----------|
| `drive`          | Full read/write to all files in your Drive                 | A general sync mount (recommended). |
| `drive.readonly` | Read-only to file metadata and contents                    | You only want inbound sync / a mirror. |
| `drive.file`     | Only files the app itself creates or opens                 | Rarely usable — see the caveat below. |

Narrower is safer; `drive` is the most convenient for a two-way mount. To change
scope later, re-run `drivel login` with a different `-scope` — the new consent
replaces the old token.

**A caveat on `drive.file`.** It sounds like the safe default and is not, for a
structural reason: the only way to grant it access to files that already exist is
through Google's Picker, a JavaScript component, and `drivel login` is a
terminal flow with no browser surface to host one. Files you already have would
be *invisible* to Drivel rather than merely read-only. The scope is plumbed and
untested; it suits a folder Drivel creates and owns from scratch, which is not
how it is used today.

## Troubleshooting

The errors this setup produces — `access_denied`, `accessNotConfigured`,
`redirect_uri_mismatch`, a login that stops working after a week, a loopback
capture that never fires — are collected in
[Troubleshooting → Login and credentials](troubleshooting.md#login-and-credentials).

## What this produces, and where it lives

`credentials.json` identifies *your project* to Google; `token.json` is your
account's access and refresh token. Both are secrets, both are written `0600`,
and both are gitignored in this repo. [Installing → Where Drivel keeps
things](install.md#where-drivel-keeps-things) has the full list of paths.

If a secret leaks, delete the OAuth client in the Cloud Console — that
invalidates every token minted from it immediately — and create a new one.

Drivel bundles no application secrets. The OAuth client you just created is the
only one it has.

## What Google sees

Drivel syncs through the OAuth client you created, so your file contents and
their metadata reach Google Drive the way any Drive client's would. What Google
does with them — including anything it counts, logs or reports on — is governed
by Google's terms and by the settings of the Cloud project you own, and is not
something this project has any say in or can speak for.

That is why it is written down here rather than in Drivel's general
documentation: it is a property of the backend you chose, so it belongs on the
backend's page. Each provider gets its own.
