package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/notify"
	"clippet/internal/urls"
)

// `clippet admin bot` — the operator's half of the Discord bot (ROADMAP #33,
// DESIGN.md §20 erratum 255; DEPLOY.md §30).
//
// Four verbs and no more: which account a server uploads into (`bind`,
// `unbind`, `ls`), and publishing the slash commands in that server
// (`register`). Since §20 erratum 315 there are two, `/upload` and `/clip`,
// unless DISCORD_UPLOAD_COMMAND_NAME and DISCORD_COMMAND_NAME renamed them
// (erratum 284), and every line these verbs print names whichever they are. There is
// no self-service path and no OAuth flow, because Clippet is single-tenant
// (§1): the operator names the account, and a server nobody named gets a
// one-line refusal.

// botRegisterTimeout bounds the ONE outbound call `register` makes per guild.
// It is generous next to internal/notify's own 10 s per-call budget, which is
// what actually cuts a hung request off; this is the CLI's ceiling on the
// whole verb.
const botRegisterTimeout = 60 * time.Second

func (a *admin) botUsage() {
	fmt.Fprintln(a.env.stderr, "usage: clippet admin bot bind <guild-id> <username> [--person] [--no-register]")
	fmt.Fprintln(a.env.stderr, "       clippet admin bot unbind <guild-id>")
	fmt.Fprintln(a.env.stderr, "       clippet admin bot ls")
	fmt.Fprintln(a.env.stderr, "       clippet admin bot register [<guild-id>]")
	fmt.Fprintln(a.env.stderr, "  <guild-id> is the Discord server's id (Developer Mode → right-click the server → Copy Server ID).")
	fmt.Fprintln(a.env.stderr, "  --person binds the server for that account's linked Discord user alone (`admin users link` first); every")
	fmt.Fprintln(a.env.stderr, "  other member's command is refused here (DEPLOY.md §30.8 step 3b).")
	fmt.Fprintln(a.env.stderr, "  bind publishes "+a.botCommands()+" in that server too, unless --no-register; register (re)publishes them in one")
	fmt.Fprintln(a.env.stderr, "  bound server, or in every bound server when no id is given. Both need DISCORD_BOT_TOKEN.")
	fmt.Fprintln(a.env.stderr, "  The names are DISCORD_UPLOAD_COMMAND_NAME (default /upload), DISCORD_COMMAND_NAME (default /clip) and")
	fmt.Fprintln(a.env.stderr, "  DISCORD_ENRICH_COMMAND_NAME (default Enrich, a message's Apps menu) and DISCORD_SETTINGS_COMMAND_NAME")
	fmt.Fprintln(a.env.stderr, "  (default /settings, the link defaults; DEPLOY.md §30.2).")
	fmt.Fprintln(a.env.stderr, "  With DISCORD_BOT_TOKEN=managed (the official bot, DEPLOY.md §30.8) bind stores the binding and")
	fmt.Fprintln(a.env.stderr, "  publishes nothing, and register is refused: /upload, /clip, Enrich and /settings are published once, by the official application.")
}

// botCommands is the four commands this instance publishes as one phrase —
// `/upload, /clip, Enrich and /settings` by default (DISCORD_UPLOAD_COMMAND_NAME,
// DISCORD_COMMAND_NAME, DISCORD_ENRICH_COMMAND_NAME and
// DISCORD_SETTINGS_COMMAND_NAME; §20 errata 284, 315, 318).
// The two slash commands carry their slash; the message command, which a person
// picks from a message's Apps menu and never types, does not. With the bot OFF
// there is no command at all, and the verbs that print them refuse before they
// reach here; the defaults are spelled for that case so no message can ever
// read "published /" with nothing after it.
func (a *admin) botCommands() string {
	upload, link, enrich := a.cfg.DiscordBot.UploadCommandName(), a.cfg.DiscordBot.CommandName(), a.cfg.DiscordBot.EnrichCommandName()
	if upload == "" {
		upload = config.DefaultUploadCommandName
	}
	if link == "" {
		link = config.DefaultCommandName
	}
	if enrich == "" {
		enrich = config.DefaultEnrichCommandName
	}
	settings := a.cfg.DiscordBot.SettingsCommandName()
	if settings == "" {
		settings = config.DefaultSettingsCommandName
	}
	// `/signin` rides along under its fixed name (§20 erratum 341): Register
	// publishes it beside the four on an own application.
	return "/" + upload + ", /" + link + ", " + enrich + ", /" + settings + " and /" + config.SigninCommandName
}

// managedNote is the one sentence the managed case prints wherever a
// registration would otherwise have happened (§20 errata 279, 315).
const managedNote = "nothing to publish: this instance answers the official Clippet bot (DISCORD_BOT_TOKEN=managed), whose /upload, /clip, Enrich and /settings are published once, globally"

func (a *admin) bot(args []string) int {
	if len(args) == 0 {
		a.botUsage()
		return 2
	}
	switch args[0] {
	case "bind":
		return a.botBind(args[1:])
	case "unbind":
		return a.botUnbind(args[1:])
	case "ls":
		return a.botLs(args[1:])
	case "register":
		return a.botRegister(args[1:])
	}
	a.botUsage()
	return 2
}

// botBind points one Discord server at one account, and — unless told not to
// — publishes the command there straight away.
//
// The registration rides along because the two are one operator intention:
// "this server uploads into that account" is not true until the command exists
// in the server, and a bind that quietly left it unpublished would look
// like the bot was broken. `--no-register` is for the case where the bot token
// cannot reach Discord from where the CLI runs (an air-gapped restore, a test
// harness); the binding is stored either way and `admin bot register` finishes
// the job later.
//
// `--person` (§20 erratum 342) binds the server for ONE member: the account's
// linked Discord user. It is refused, in the operator's words, while the
// account has no linked id — a person binding without one would refuse every
// member, silently. A rebind writes the scope it is given: the same command
// without `--person` makes the server every member's again.
func (a *admin) botBind(args []string) int {
	fs := a.flags("bot bind")
	noRegister := fs.Bool("no-register", false, "store the binding without publishing the command in that server")
	person := fs.Bool("person", false, "bind the server for the account's linked Discord user alone")
	pos, err := parsePositionals(fs, args, 2)
	if err != nil || len(pos) != 2 {
		a.botUsage()
		return 2
	}
	guildID, name := strings.TrimSpace(pos[0]), strings.TrimSpace(pos[1])
	if !validGuildID(guildID) {
		fmt.Fprintf(a.env.stderr, "admin bot bind: %q is not a Discord server id (15-25 digits)\n", guildID)
		return 2
	}
	u, ok := a.resolveUser(name)
	if !ok {
		return 1
	}
	scope, scopeNote := db.GuildScopeServer, ""
	if *person {
		if u.DiscordID == nil || *u.DiscordID == "" {
			fmt.Fprintf(a.env.stderr, "admin bot bind: %s has no Discord user id - run `clippet admin users link %s <discord-id>` first\n", u.Username, u.Username)
			return 1
		}
		scope, scopeNote = db.GuildScopePerson, " (person)"
	}
	rebound, err2 := a.db.BindGuildScope(a.ctx, guildID, u.ID, scope, a.env.now().Unix())
	if err2 != nil {
		return a.fail("bot bind", err2)
	}
	verb := "bound"
	if rebound {
		verb = "rebound"
	}
	fmt.Fprintf(a.env.stdout, "guild %s %s to %s (%s)%s\n", guildID, verb, u.Username, u.ID, scopeNote)
	if a.cfg.DiscordBot.Managed() {
		// The official bot: the binding IS the whole operator action, and the
		// claim page's origin check (GET /api/bot/claim) reads it from now on.
		fmt.Fprintln(a.env.stdout, managedNote+"; the claim page's origin check answers 200 for this server from now on")
		return 0
	}
	if *noRegister {
		fmt.Fprintln(a.env.stdout, a.botCommands()+" were NOT published in that server (--no-register): run `clippet admin bot register "+guildID+"` when you can reach Discord")
		return 0
	}
	return a.registerGuilds([]string{guildID})
}

func (a *admin) botUnbind(args []string) int {
	fs := a.flags("bot unbind")
	guildID, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(guildID) == "" {
		a.botUsage()
		return 2
	}
	guildID = strings.TrimSpace(guildID)
	had, err := a.db.UnbindGuild(a.ctx, guildID)
	if err != nil {
		return a.fail("bot unbind", err)
	}
	if !had {
		// Not an error: an operator who typed the id wrong needs to be told,
		// and one who ran it twice needs nothing to happen.
		fmt.Fprintf(a.env.stdout, "guild %s: no binding\n", guildID)
		return 0
	}
	fmt.Fprintf(a.env.stdout, "guild %s unbound; %s there are refused from now on (the commands themselves stay published until Discord is told otherwise)\n", guildID, a.botCommands())
	return 0
}

func (a *admin) botLs(args []string) int {
	fs := a.flags("bot ls")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		a.botUsage()
		return 2
	}
	rows, err := a.db.ListGuildBindings(a.ctx)
	if err != nil {
		return a.fail("bot ls", err)
	}
	for _, g := range rows {
		// The fourth column is the scope (§20 erratum 342): `server` or `person`.
		fmt.Fprintf(a.env.stdout, "%s  %s  user=%s bound=%s  %s\n", g.GuildID, g.Username, g.UserID, rfc(g.CreatedAt), g.Scope)
	}
	fmt.Fprintf(a.env.stdout, "%d binding(s)\n", len(rows))
	if !a.cfg.DiscordBot.Configured() {
		fmt.Fprintln(a.env.stdout, "discord bot: off (DISCORD_APP_ID, DISCORD_PUBLIC_KEY and DISCORD_BOT_TOKEN are unset) - a binding does nothing until it is on")
	} else if a.cfg.DiscordBot.Managed() {
		fmt.Fprintln(a.env.stdout, "discord bot: managed (this instance answers the official bot; nothing is registered from here)")
	}
	return 0
}

// botRegister publishes this instance's four commands in one bound guild, or in
// every one — in ONE PUT per guild, which replaces that guild's whole set.
//
// GUILD commands rather than global ones, always: a guild command is live the
// moment this returns, where a global command takes up to an hour to reach
// every client. There is no "unregister": unbinding stops `/clip` from doing
// anything (the endpoint refuses it), and removing the command itself is a
// thing to do in the developer portal on the rare occasion it matters.
func (a *admin) botRegister(args []string) int {
	fs := a.flags("bot register")
	guildID, err := parseWithID(fs, args)
	if err != nil {
		a.botUsage()
		return 2
	}
	guildID = strings.TrimSpace(guildID)
	if a.cfg.DiscordBot.Managed() {
		// Refused HERE, in words, rather than by Discord's `HTTP 401` to a
		// sentinel sent as a token (§20 erratum 279).
		fmt.Fprintln(a.env.stderr, "admin bot register: "+managedNote+" (DEPLOY.md §30.8)")
		return 2
	}
	if guildID != "" {
		if !validGuildID(guildID) {
			fmt.Fprintf(a.env.stderr, "admin bot register: %q is not a Discord server id (15-25 digits)\n", guildID)
			return 2
		}
		if _, err := a.db.GuildBinding(a.ctx, guildID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				fmt.Fprintf(a.env.stderr, "admin bot register: guild %s is not bound - run `clippet admin bot bind %s <username>` first\n", guildID, guildID)
				return 2
			}
			return a.fail("bot register", err)
		}
		return a.registerGuilds([]string{guildID})
	}
	rows, err := a.db.ListGuildBindings(a.ctx)
	if err != nil {
		return a.fail("bot register", err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.env.stdout, "no bindings; nothing to register")
		return 0
	}
	ids := make([]string, 0, len(rows))
	for _, g := range rows {
		ids = append(ids, g.GuildID)
	}
	return a.registerGuilds(ids)
}

// registerGuilds is the one place the CLI reaches Discord. It builds the same
// Bot serve.go builds — same host allowlist, same single client, same token
// hygiene — because a second way to make that call is a second thing to audit.
func (a *admin) registerGuilds(guilds []string) int {
	if !a.cfg.DiscordBot.Configured() {
		fmt.Fprintln(a.env.stderr, "admin bot register: the bot is off - set DISCORD_APP_ID, DISCORD_PUBLIC_KEY and DISCORD_BOT_TOKEN (DEPLOY.md §30)")
		return 2
	}
	bot := notify.NewBot(notify.BotDeps{
		AppID: a.cfg.DiscordBot.AppID(), Token: a.cfg.DiscordBot.Token(),
		Managed:             a.cfg.DiscordBot.Managed(),
		CommandName:         a.cfg.DiscordBot.CommandName(),
		UploadCommandName:   a.cfg.DiscordBot.UploadCommandName(),
		EnrichCommandName:   a.cfg.DiscordBot.EnrichCommandName(),
		SettingsCommandName: a.cfg.DiscordBot.SettingsCommandName(),
		URLs:                urls.New(a.cfg.PublicBaseURL, "admin"),
		APIHost:             config.DiscordAPIHost(),
	})
	if bot == nil { // unreachable: Configured() is the same condition
		fmt.Fprintln(a.env.stderr, "admin bot register: the bot is off")
		return 2
	}
	ctx, cancel := context.WithTimeout(a.ctx, botRegisterTimeout)
	defer cancel()
	failed := 0
	for _, g := range guilds {
		if err := bot.Register(ctx, g); err != nil {
			// The error is already scrubbed of the bot token by
			// internal/notify; it names a status and Discord's own JSON,
			// which is what makes a 401 ("invalid token") or a 403 ("missing
			// access") diagnosable.
			fmt.Fprintf(a.env.stderr, "admin bot register: guild %s: %v\n", g, err)
			failed++
			continue
		}
		fmt.Fprintf(a.env.stdout, "guild %s: %s published (guild commands are live immediately)\n", g, a.botCommands())
	}
	if failed > 0 {
		fmt.Fprintf(a.env.stderr, "%d of %d guild(s) failed; the binding(s) are stored - fix the token or the install and re-run `clippet admin bot register`\n",
			failed, len(guilds))
		return 1
	}
	return 0
}

// validGuildID applies the same shape DISCORD_APP_ID is held to: a snowflake
// is 15-25 digits. It is a typo guard, not a security control — an id that
// passes and does not exist simply never matches an interaction.
func validGuildID(s string) bool {
	if len(s) < 15 || len(s) > 25 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
