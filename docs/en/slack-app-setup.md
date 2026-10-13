# Creating the Slack app

A step-by-step guide to creating the Slack app for the Office Attendance Bot. You can do this without knowing anything about the bot's operation or Docker. At the end you hand over a handful of values for `.env.local` to whoever runs the bot.

Time required: about 10 minutes. 🇩🇪 [Deutsche Version](../de/slack-app-setup.md)

---

## Before you start

- You need permission to install apps in the Slack workspace. In many workspaces only admins can do this. Otherwise Slack offers **Request to Install**, and an admin has to approve the request.
- Decide on a **results channel** where the weekly summary will appear, e.g. `#office`.
- Decide **who is asked**:
  - **Mode A:** all members of a channel, e.g. `#team-berlin`
  - **Mode B:** a fixed list of people

The app configuration pages at api.slack.com are in English. The labels below refer to them.

---

## Step 1: Create the app from the manifest

1. Open <https://api.slack.com/apps> and sign in.
2. Click **Create New App** → **From a manifest**.
3. Choose the workspace and click **Next**.
4. Switch to the **YAML** tab, replace the content with the following manifest and click **Next** → **Create**.

```yaml
display_information:
  name: Office Planning
  description: Weekly poll of office days
features:
  bot_user:
    display_name: office-planning
    always_online: true
  app_home:
    home_tab_enabled: false
    messages_tab_enabled: true
    messages_tab_read_only_enabled: true
oauth_config:
  scopes:
    bot:
      - chat:write
      - im:write
      - im:history
      - users:read
      - channels:read
      - groups:read
      - channels:history
      - groups:history
settings:
  interactivity:
    is_enabled: true
  socket_mode_enabled: true
  org_deploy_enabled: false
  token_rotation_enabled: false
```

The name "Office Planning" (`name`) is what people see in Slack. The bot is mentioned and invited by its `display_name`, i.e. `@office-planning`. The `display_name` must not contain spaces or non-ASCII characters such as umlauts; lowercase letters, digits, `-`, `_` and `.` are the safe choice. You can change both before creating the app, e.g. to `name: Büroplanung` and `display_name: bueroplanung` for a German team (`BOT_LANGUAGE=de`). In that case, use the new `display_name` wherever this guide says `@office-planning`.

The manifest already enables Socket Mode (no public URL needed) and interactivity (the **Save** button and the **Plan time off** form). The form needs no additional permissions. It also sets all required permissions. For what each permission is used for, see [Required OAuth scopes](README.md#required-oauth-scopes).

---

## Step 2: Create the app-level token (`xapp-…`)

The bot uses this token to open its Socket Mode connection to Slack.

1. In the app, go to **Settings → Basic Information**.
2. Under **App-Level Tokens**, click **Generate Token and Scopes**.
3. Enter a name, e.g. `socket`. Click **Add Scope** and choose **`connections:write`**, then click **Generate**.
4. Copy the token that starts with `xapp-`. It becomes **`SLACK_APP_TOKEN`**.

---

## Step 3: Check interactivity and Socket Mode

The manifest already sets both. A quick check:

- **Settings → Socket Mode**: *Enable Socket Mode* must be on.
- **Features → Interactivity & Shortcuts**: *Interactivity* must be on. No request URL is needed.

---

## Step 4: Install the app and copy the bot token (`xoxb-…`)

1. Go to **Settings → Install App** and click **Install to Workspace** (or **Request to Install**).
2. Confirm the permissions with **Allow**.
3. Copy the **Bot User OAuth Token** that starts with `xoxb-`. It becomes **`SLACK_BOT_TOKEN`**.

> If you change permissions later, Slack shows a yellow banner. The app must then be **reinstalled** before the change takes effect.

---

## Step 5: Invite the bot to the channels

In Slack (not on api.slack.com):

- **Results channel:** type `/invite @office-planning` in the channel. Without this, the bot is not allowed to post the summary.
- **Participants channel (mode A only):** run `/invite @office-planning` there too, so the bot may read the member list.

Private channels work as well. The bot just has to be invited.

---

## Step 6: Find the IDs

The bot needs IDs, not names.

- **Channel ID** (`C…`): open the channel, click its name at the top. The ID is shown at the very bottom of the dialog, with a copy button.
- **User ID** (`U…`, mode B only): click a person's name → **View full profile** → **⋮** (more) → **Copy member ID**.
- **Many user IDs at once:** once the bot token is in `.env.local`, `docker compose run --rm officebot list-users` lists all active people with their IDs. It ends with a ready-to-paste `SLACK_USER_IDS=…` line. Add `--channel C…` to list only the members of one channel. Bots and deactivated accounts are left out; `--all` shows them too.

---

## Step 7: Hand over the values

Put these values into `.env.local` (template: `.env.local.example`), or pass them on to whoever runs the bot. **The tokens are secrets.** Share them only through a secure channel, never in Slack or by email in plain text, and never commit them to git.

```dotenv
SLACK_BOT_TOKEN=xoxb-...            # step 4
SLACK_APP_TOKEN=xapp-...            # step 2
SLACK_SUMMARY_CHANNEL_ID=C...       # step 6, results channel

# Mode A: all members of a channel
SLACK_PARTICIPANTS_CHANNEL_ID=C...  # step 6, participants channel
# Mode B: explicit list (leave SLACK_PARTICIPANTS_CHANNEL_ID empty)
SLACK_USER_IDS=U...,U...
```

Set **either** `SLACK_PARTICIPANTS_CHANNEL_ID` **or** `SLACK_USER_IDS`, not both.

---

## Step 8: Quick check

Once the bot is running (`docker compose up -d --build`):

1. `docker compose ps` shows the container as **healthy**. That means the Socket Mode connection works.
2. Trigger the poll: `docker compose exec officebot /officebot start-week`. Within about 30 s, everyone asked receives a DM from **Office Planning**, and the summary appears in the results channel.
3. Tick a day in the DM and click **Save**. The DM confirms with "✅ Saved", and the summary updates.

For a first test, it is best to set `SLACK_USER_IDS` to your own ID only, and to use a test channel as the results channel.

---

## Common problems

| Problem                                     | Fix                                                                                                           |
|---------------------------------------------|---------------------------------------------------------------------------------------------------------------|
| Log: `invalid_auth` / `not_authed`          | Wrong or truncated `SLACK_BOT_TOKEN`. Copy it again from **Install App**.                                     |
| Log: `socket mode: invalid app token`       | `SLACK_APP_TOKEN` is wrong or lacks `connections:write`. Generate a new one (step 2).                         |
| Log: `missing_scope`                        | A permission is missing. Compare with the manifest (**Features → App Manifest**), then **reinstall** the app. |
| Log: `not_in_channel` / `channel_not_found` | Invite the bot to the channel (step 5) and check the channel ID (step 6).                                     |
| No participants found (mode A)              | The bot is not in the participants channel, or the channel only contains bots/deactivated users.              |
| Clicking **Save** does nothing              | Check that interactivity is on (step 3) and that the container is healthy.                                    |
| The app cannot be installed                 | The workspace requires admin approval. Use **Request to Install** and ask an admin.                           |

More troubleshooting: [README → Troubleshooting](README.md#troubleshooting).
