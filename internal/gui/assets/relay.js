// AI Relay's two pages (relay.go). Mag, the member's client, has Hub: the
// hub this computer is connected to, who the member is there, the models
// they may use and their use, read from the hub's own ledger. Pie, the
// server, has Members: the members, their groups, plans, devices and use.
// Which program this is comes with boot.js (bootPrefs.relay); the other
// program's page is never drawn, and its routes answer 403 whoever asks.
(() => {
  const relay = window.bootPrefs?.relay || "";
  if (!relay) return;
  // these pages' own words first (relay-i18n.js), then the app's
  const t = (s, vars) => {
    let out = window.RELAY_I18N?.[locale]?.[s] ?? I18N[locale]?.[s] ?? s;
    if (vars) for (const k in vars) out = out.split("{" + k + "}").join(vars[k]);
    return out;
  };

  const fmtN = (n) => Number(n || 0).toLocaleString();
  const usd = (n) => "$" + Number(n || 0).toFixed(Number(n || 0) > 0 && Number(n) < 0.01 ? 4 : 2);
  const when = (s) => { const d = new Date(typeof s === "number" ? s * 1000 : s); return isNaN(d) ? "" : d.toLocaleString(undefined, { hour12: false }); };
  const PERIODS = [["today", "Today"], ["7d", "7 days"], ["30d", "30 days"], ["month", "This month"], ["all", "All"]];
  const WINDOWS = [["day", "day"], ["week", "week"], ["month", "month"]];

  function head(label, ...right) {
    const h = el("div", "row-head");
    h.append(el("span", "label", t(label)), el("span", "grow"), ...right);
    return h;
  }
  function btn(text, cls, on) {
    const b = el("button", "text " + (cls || ""), t(text));
    b.type = "button";
    b.onclick = on;
    return b;
  }
  // choice is a row of buttons of which one is on: the app has no <select>
  function choice(opts, value, on) {
    const box = el("div", "rl-choice");
    box.setAttribute("role", "radiogroup");
    for (const [v, label] of opts) {
      const b = el("button", "text" + (v === value ? " on" : ""), t(label));
      b.type = "button";
      b.setAttribute("role", "radio");
      b.setAttribute("aria-checked", String(v === value));
      b.onclick = () => { if (v !== value) on(v); };
      box.append(b);
    }
    return box;
  }
  function field(ed, label, control, note) {
    const l = el("label", "rl-l", t(label));
    const wrap = el("div", "rl-c");
    wrap.append(control);
    if (note) wrap.append(el("div", "rl-note", t(note)));
    if (control.matches?.("input, textarea")) { control.id = control.id || "rl-" + Math.random().toString(36).slice(2, 9); l.htmlFor = control.id; }
    ed.append(l, wrap);
    return control;
  }
  function input(value, type, placeholder) {
    const i = el("input", "rl-in");
    i.type = type || "text";
    i.value = value ?? "";
    i.autocomplete = "off";
    i.spellcheck = false;
    if (placeholder) i.placeholder = t(placeholder);
    return i;
  }
  function editor(title) {
    const ed = el("div", "editor rl-editor");
    const h = el("div", "ehead");
    h.append(el("b", "", t(title)));
    ed.append(h);
    return ed;
  }
  function bar(ed, go, extra) {
    const b = el("div", "bar");
    const err = el("span", "rl-err");
    err.setAttribute("role", "alert");
    const cancel = btn("Cancel", "", () => closeModal());
    b.append(err, el("span", "grow"), ...(extra || []), cancel, go);
    ed.append(b);
    return err;
  }
  function totalsCells(tt) {
    return [fmtN(tt.calls), fmtN(tt.input + tt.est), fmtN(tt.output), fmtN(tt.cacheRead), usd(tt.charge)];
  }
  function table(cols, rows) {
    const wrap = el("div", "rl-scroll");
    const tb = el("table", "rl-table");
    const hr = el("tr");
    for (const c of cols) { const th = el("th", c.num ? "num" : "", t(c.label)); hr.append(th); }
    tb.append(hr);
    for (const r of rows) {
      const tr = el("tr");
      r.forEach((cell, i) => {
        const td = el("td", cols[i].num ? "num" : cols[i].mono ? "mono" : "");
        if (cell instanceof Node) td.append(cell); else td.textContent = cell ?? "";
        tr.append(td);
      });
      tb.append(tr);
    }
    wrap.append(tb);
    return wrap;
  }
  function limitLine(l) {
    if (!l) return null;
    const box = el("div", "rl-limit");
    const rows = [];
    if (l.tokenLimit) rows.push([t("{used} of {limit} tokens this {period}", { used: fmtN(l.tokens), limit: fmtN(l.tokenLimit), period: t(l.period) }), l.tokens / l.tokenLimit]);
    if (l.chargeLimit) rows.push([t("{used} of {limit} charged this {period}", { used: usd(l.charge), limit: usd(l.chargeLimit), period: t(l.period) }), l.charge / l.chargeLimit]);
    if (l.costLimit) rows.push([t("{used} of {limit} estimated cost this {period}", { used: usd(l.cost), limit: usd(l.costLimit), period: t(l.period) }), l.cost / l.costLimit]);
    for (const [text, part] of rows) {
      const meter = el("div", "rl-bar");
      const fill = el("i");
      fill.style.width = Math.max(0, Math.min(100, part * 100)).toFixed(1) + "%";
      if (part >= 1) fill.classList.add("full");
      meter.append(fill);
      box.append(el("div", "rl-limit-text", text + " · " + t("resets {when}", { when: when(l.reset) })), meter);
    }
    if (l.inFlight) box.append(el("div", "rl-note", t("{n} in flight, {tokens} tokens reserved", { n: l.inFlight, tokens: fmtN(l.reserved) })));
    return box;
  }
  function planText(p) {
    if (!p) return t("No plan");
    if (p.kind === "metered") {
      return t("{in} in · {out} out per million tokens", { in: usd(p.priceIn), out: usd(p.priceOut) }) + (p.budget ? " · " + t("at most {n} a {window}", { n: usd(p.budget), window: t(p.window) }) : "");
    }
    return t("{price} a {window}", { price: usd(p.price), window: t(p.window) }) + " · " + (p.tokens ? t("{n} tokens included", { n: fmtN(p.tokens) }) : t("no token limit"));
  }
  async function copyOut(text, done) {
    try { await navigator.clipboard.writeText(text); status(t(done || "Copied"), "ok"); }
    catch { try { await api("copy", { text }); status(t(done || "Copied"), "ok"); } catch (e) { status(e.message, "err"); } }
  }
  function usageBlock(st, admin) {
    const box = el("div", "rl-usage");
    const tiles = el("div", "rl-tiles");
    const tile = (label, value, note) => { const d = el("div", "rl-tile"); d.append(el("span", "rl-k", t(label)), el("b", "", value)); if (note) d.append(el("span", "rl-k", note)); tiles.append(d); };
    tile("Requests", fmtN(st.calls), st.refused ? t("{n} refused", { n: fmtN(st.refused) }) : "");
    tile("Input tokens", fmtN(st.input));
    tile("Output tokens", fmtN(st.output));
    tile("Cache-read tokens", fmtN(st.cacheRead));
    if (st.charge || st.fee) {
      tile("Charged", usd(st.charge));
      if (st.fee) tile("Plan fee", usd(st.fee));
      tile("Total", usd(st.due));
    }
    if (admin && st.cost) tile("Estimated upstream cost", usd(st.cost));
    box.append(tiles);
    if (st.unknown) box.append(el("p", "usage-note", t("{n} request(s) reported no usage and were counted at the {est} tokens reserved for them.", { n: fmtN(st.unknown), est: fmtN(st.est) })));
    if (!st.byModel?.length) { box.append(el("div", "none", t("Nothing in this time yet"))); return box; }
    box.append(table([{ label: "Model", mono: true }, { label: "Requests", num: true }, { label: "In", num: true }, { label: "Out", num: true }, { label: "Cache read", num: true }, { label: "Charged", num: true }],
      st.byModel.map((l) => [l.key, ...totalsCells(l)])));
    const settle = { settled: "as reported", estimated: "estimated", released: "not counted", interrupted: "interrupted" };
    box.append(el("div", "rl-sub", t("Latest requests")));
    box.append(table([{ label: "Time" }, { label: "Model", mono: true }, { label: "Device" }, { label: "In", num: true }, { label: "Out", num: true }, { label: "Status" }, { label: "Counted" }, { label: "Charged", num: true }],
      st.recent.slice(0, 50).map((e) => {
        const s = el("span");
        const dot = el("span", "rl-dot " + (e.status >= 200 && e.status < 300 ? "ok" : e.refused ? "warn" : "bad"));
        s.append(dot, String(e.status || "—"));
        return [when(e.time), e.model, e.device || "", fmtN(e.input + (e.est || 0)), fmtN(e.output), s, t(e.refused ? "refused" : settle[e.settle] || "as reported"), usd(e.charge)];
      })));
    return box;
  }
  function periods(value, on) { return choice(PERIODS, value, on); }

  // ------------------------------------------------------------------ Mag
  if (relay === "client") {
    const page = $("#view-hub");
    if (!page) return;
    let st = null, usage = null, failed = "", period = "30d", loading = 0, busy = false;
    const form = { address: "", how: "login", name: "", password: "", code: "", key: "", device: "" };

    async function load() {
      const n = ++loading;
      if (!st) draw();
      try {
        const d = await api("hub");
        if (n !== loading) return;
        st = d; failed = "";
        if (!form.device) form.device = d.name || "";
        if (!form.address && d.address) form.address = d.address;
        usage = null;
        if (d.connected && !d.error) {
          try { usage = await api("hub/usage?period=" + encodeURIComponent(period)); } catch (e) { failed = e.message; }
          if (n !== loading) return;
        }
      } catch (e) {
        if (n !== loading) return;
        failed = e.message;
      }
      if (view === "hub") draw();
    }
    window.loadHubPage = load;

    function connectForm() {
      const box = el("div", "list rl-card");
      const ed = el("div", "rl-form");
      const addr = field(ed, "Hub address", input(form.address, "text", "https://relay.example.org or http://192.168.1.20:3425"));
      addr.oninput = () => { form.address = addr.value; };
      ed.append(el("label", "rl-l", t("Connect with")), (() => {
        const w = el("div", "rl-c");
        w.append(choice([["login", "Name and password"], ["invite", "Invitation code"], ["key", "Device key"]], form.how, (v) => { form.how = v; draw(); }));
        return w;
      })());
      if (form.how === "login") {
        const n = field(ed, "Name", input(form.name)); n.autocomplete = "username"; n.oninput = () => { form.name = n.value; };
        const p = field(ed, "Password", input(form.password, "password")); p.autocomplete = "current-password"; p.oninput = () => { form.password = p.value; };
      } else if (form.how === "invite") {
        const c = field(ed, "Invitation code", input(form.code), "The code your administrator sent you. It works once."); c.oninput = () => { form.code = c.value; };
        const p = field(ed, "New password", input(form.password, "password"), "At least 10 characters. You sign in to the hub's page with it later."); p.autocomplete = "new-password"; p.oninput = () => { form.password = p.value; };
      } else {
        const k = field(ed, "Device key", input(form.key, "password"), "A key you made on the hub's page, under Devices."); k.oninput = () => { form.key = k.value; };
      }
      if (form.how !== "key") {
        const d = field(ed, "This device", input(form.device), "What this computer is called at the hub. Its key can be revoked there without touching your other devices."); d.oninput = () => { form.device = d.value; };
      }
      const err = el("div", "rl-err"); err.setAttribute("role", "alert");
      const go = btn("Connect", "primary", async () => {
        if (busy) return;
        busy = true; go.disabled = true; err.textContent = "";
        try {
          st = await api("hub/connect", form);
          form.password = form.code = form.key = ""; // used once, kept nowhere
          status(t("Connected to the hub"), "ok");
          busy = false;
          await load();
          return;
        } catch (e) { err.textContent = t(e.message); }
        busy = false; go.disabled = false;
      });
      const row = el("div", "rl-actions");
      row.append(err, el("span", "grow"), go);
      box.append(ed, row);
      return box;
    }

    function draw() {
      const out = [];
      if (!st) { out.push(el("div", "none", failed ? t(failed) : t("Loading…"))); page.replaceChildren(...out); return; }
      if (!st.connected) {
        out.push(head("Connect to a hub"));
        out.push(el("p", "usage-note", t("Mag keeps no AI account of its own. It connects this computer to your group's hub as one of your devices, and the agents here use the models the hub lets you use.")));
        out.push(connectForm());
        page.replaceChildren(...out);
        return;
      }
      const refresh = btn("Refresh", "", () => load());
      const off = btn("Disconnect", "danger", () => askDisconnect());
      out.push(head("Hub", refresh, off));
      const card = el("div", "list rl-card");
      const top = el("div", "rl-top");
      top.append(el("span", "rl-dot " + (st.error ? "bad" : "ok")), el("b", "mono", st.address));
      if (st.member) top.append(el("span", "rl-chip", st.member.display ? st.member.display + " · " + st.member.name : st.member.name), el("span", "rl-k", t("device {name}", { name: st.device || "—" })));
      card.append(top);
      if (st.plain) card.append(el("p", "rl-warn", t("This address is plain http: the device key crosses the network unencrypted. Use it only over a VPN or a network you trust.")));
      if (st.error) {
        card.append(el("p", "rl-warn", st.revoked ? t("The hub no longer takes this computer's device key: it was revoked, or your account was turned off. Connect again to make a new one.") : t(st.error)));
        out.push(card);
        if (st.revoked) { out.push(head("Connect again")); out.push(connectForm()); }
        page.replaceChildren(...out);
        return;
      }
      const m = st.member;
      const facts = el("div", "rl-tiles");
      const fact = (k, v, note) => { const d = el("div", "rl-tile"); d.append(el("span", "rl-k", t(k)), el("b", "", v)); if (note) d.append(el("span", "rl-k", note)); facts.append(d); };
      fact("Groups", m.groups.length ? m.groups.map((g) => g.name).join(", ") : "—");
      fact("Plan", m.plan ? m.plan.name : t("None"), m.plan ? planText(m.plan) : "");
      fact("Requests at once", String(m.concurrency), t("{n} more may wait", { n: m.queue }));
      card.append(facts);
      const lim = limitLine(m.limit);
      card.append(lim || el("p", "usage-note", t("No limit is set for your account.")));
      out.push(card);

      out.push(head("Models you may use"));
      out.push(el("p", "usage-note", t("Pick one for an agent on the Agents page. The local gateway at {url} sends its requests to the hub with this device's key.", { url: st.gateway })));
      const models = el("div", "list rl-models");
      if (!st.models?.length) models.append(el("div", "none", t("No model has been given to you yet. Ask your administrator.")));
      for (const mo of st.models || []) {
        const r = el("div", "rl-model");
        const id = "hub/" + mo.id;
        r.append(el("span", "mono", id));
        if (mo.group) r.append(el("span", "rl-chip", t("routing group")));
        r.append(el("span", "grow"), btn("Copy", "quiet", () => copyOut(id)));
        models.append(r);
      }
      out.push(models);

      out.push(head("Your usage", periods(period, (p) => { period = p; load(); })));
      out.push(el("p", "usage-note", t("From the hub's ledger: the same records its administrator's totals are of. Requests are counted, never what was asked or answered.")));
      if (usage) out.push(usageBlock(usage, false));
      else out.push(el("div", "none", failed ? t(failed) : t("Loading…")));
      page.replaceChildren(...out);
    }

    function askDisconnect() {
      const ed = editor("Disconnect from the hub?");
      ed.append(el("p", "lib-confirm", t("This computer forgets the hub and its device key. The device stays listed at the hub until it is revoked there.")));
      const go = btn("Disconnect", "primary", async () => {
        go.disabled = true;
        try { st = await api("hub/disconnect", {}); usage = null; closeModal(); draw(); }
        catch (e) { err.textContent = t(e.message); go.disabled = false; }
      });
      const err = bar(ed, go);
      openModal(ed);
      $("#modal").classList.add("lib");
    }
    // the page was opened before this script had loaded: app.js shows the
    // first tab as it starts
    if (view === "hub") load();
    return;
  }

  // ------------------------------------------------------------------ Pie
  const page = $("#view-members");
  if (!page) return;
  let st = null, failed = "", period = "30d", loading = 0;
  const open = new Set(); // members whose devices and use are unfolded

  async function load() {
    const n = ++loading;
    if (!st) draw();
    try {
      const d = await api("relay?period=" + encodeURIComponent(period));
      if (n !== loading) return;
      st = d; failed = "";
    } catch (e) {
      if (n !== loading) return;
      failed = e.message;
    }
    if (view === "members") draw();
  }
  window.loadMembersPage = load;

  async function act(action, body, errBox) {
    try {
      const out = await api("relay/" + action, { ...body, period });
      st = out.state;
      draw();
      return out;
    } catch (e) {
      if (errBox) errBox.textContent = t(e.message); else status(t(e.message), "err");
      return null;
    }
  }
  const memberState = (m) => m.off ? ["off", "Turned off"] : m.hasPassword ? ["ok", "Active"] : m.invited ? ["warn", "Invited"] : ["", "Not invited yet"];

  function draw() {
    const out = [];
    if (!st) { out.push(el("div", "none", failed ? t(failed) : t("Loading…"))); page.replaceChildren(...out); return; }
    const info = el("div", "list rl-card");
    const top = el("div", "rl-top");
    top.append(el("span", "rl-k", t("Members reach the hub at")), el("b", "mono", st.hub), el("span", "grow"),
      btn("Copy sign-in page", "quiet", () => copyOut(st.page)));
    info.append(top);
    if (st.local) info.append(el("p", "rl-warn", t("The hub listens on this computer only. Start it with pie serve --listen 0.0.0.0:<port> (or a VPN address), behind a VPN or an HTTPS reverse proxy, for other computers to reach it.")));
    out.push(info);

    out.push(head("Members", btn("Add member", "", () => editMember(null))));
    const list = el("div", "accts rl-members");
    if (!st.members.length) list.append(el("div", "none", t("No members yet. Add one, then send them an invitation.")));
    for (const m of st.members) {
      const row = el("div", "acc rl-member" + (m.off ? " off" : ""));
      const [cls, label] = memberState(m);
      const fold = el("button", "rl-fold" + (open.has(m.id) ? " open" : ""));
      fold.type = "button";
      fold.setAttribute("aria-expanded", String(open.has(m.id)));
      fold.setAttribute("aria-label", t("Devices and usage of {name}", { name: m.name }));
      fold.append(svg("M6 4l4 4-4 4", 10, 1.8));
      fold.onclick = () => { open.has(m.id) ? open.delete(m.id) : open.add(m.id); draw(); };
      const name = el("span", "n", m.display ? m.display + " · " + m.name : m.name);
      const state = el("span", "rl-state");
      state.append(el("span", "rl-dot " + cls), t(label));
      row.append(fold, name, state);
      for (const g of m.groups || []) row.append(el("span", "rl-chip", (st.groups.find((x) => x.id === g) || { name: g }).name));
      const plan = st.plans.find((p) => p.id === m.plan);
      if (plan) row.append(el("span", "plan", plan.name));
      const s = st.standing[m.id], lane = st.lanes[m.id];
      row.append(el("span", "grow"));
      if (lane?.busy || lane?.waiting) row.append(el("span", "rl-k", t("{busy} running, {waiting} waiting", { busy: lane.busy, waiting: lane.waiting })));
      if (s?.limit) {
        const l = s.limit;
        const text = l.tokenLimit ? fmtN(l.tokens) + " / " + fmtN(l.tokenLimit) : l.chargeLimit ? usd(l.charge) + " / " + usd(l.chargeLimit) : usd(l.cost) + " / " + usd(l.costLimit);
        row.append(el("span", "rl-k" + (l.spent ? " spent" : ""), text));
      }
      row.append(btn("Invite", "quiet", () => invite(m)), btn("Edit", "quiet", () => editMember(m)), btn("Remove", "quiet danger", () => removeMember(m)));
      list.append(row);
      if (open.has(m.id)) list.append(memberDetail(m, s));
    }
    out.push(list);

    out.push(head("Groups", btn("Add group", "", () => editGroup(null))));
    out.push(el("p", "usage-note", t("A group says which models its members may use. A member in no group may use none.")));
    const groups = el("div", "accts");
    if (!st.groups.length) groups.append(el("div", "none", t("No groups yet")));
    for (const g of st.groups) {
      const row = el("div", "acc");
      row.append(el("span", "n", g.name), el("span", "rl-k mono rl-ellip", (g.models || []).join(", ") || t("no models")), el("span", "grow"),
        el("span", "rl-k", t("{n} member(s)", { n: st.members.filter((m) => (m.groups || []).includes(g.id)).length })),
        btn("Edit", "quiet", () => editGroup(g)), btn("Remove", "quiet danger", () => confirmRemove(t("Remove the group {name}?", { name: g.name }), t("Its members lose the models it alone gave them, from their next request."), "group-remove", g.id)));
      groups.append(row);
    }
    out.push(groups);

    out.push(head("Plans", btn("Add plan", "", () => editPlan(null))));
    out.push(el("p", "usage-note", t("A plan says what a member's use costs and how much it may be. You give members their plans here: nothing is paid through the hub.")));
    const plans = el("div", "accts");
    if (!st.plans.length) plans.append(el("div", "none", t("No plans yet")));
    for (const p of st.plans) {
      const row = el("div", "acc");
      row.append(el("span", "n", p.name), el("span", "plan", t(p.kind === "metered" ? "Metered" : "Per period")), el("span", "rl-k", planText(p)), el("span", "grow"),
        btn("Edit", "quiet", () => editPlan(p)), btn("Remove", "quiet danger", () => confirmRemove(t("Remove the plan {name}?", { name: p.name }), t("A plan a member is on can't be removed: give them another first."), "plan-remove", p.id)));
      plans.append(row);
    }
    out.push(plans);

    out.push(head("Usage", periods(period, (p) => { period = p; load(); })));
    out.push(el("p", "usage-note", t("Every member's use, from the one ledger their own pages read. A request is counted by its tokens, never kept by what was asked or answered.")));
    if (!st.usage.length) out.push(el("div", "none", t("Nothing in this time yet")));
    else out.push(table([{ label: "Member" }, { label: "Requests", num: true }, { label: "In", num: true }, { label: "Out", num: true }, { label: "Cache read", num: true }, { label: "Charged", num: true }, { label: "Counted at reservation", num: true }, { label: "Refused", num: true }, { label: "Est. upstream", num: true }],
      [...st.usage, { name: t("All"), ...st.all }].map((l) => [l.name || l.key, ...totalsCells(l), fmtN(l.unknown + l.interrupted), fmtN(l.refused), usd(l.cost)])));
    page.replaceChildren(...out);
  }

  function memberDetail(m, s) {
    const box = el("div", "rl-detail");
    if (s?.limit) box.append(limitLine(s.limit));
    box.append(el("div", "rl-sub", t("Devices")));
    if (!m.devices.length) box.append(el("div", "rl-k", t("No device yet: the member makes one after signing in.")));
    for (const d of m.devices) {
      const r = el("div", "rl-device");
      r.append(el("span", "", d.name), el("span", "plan mono", d.masked), el("span", "rl-k", d.created ? when(d.created) : ""), el("span", "grow"),
        btn("Revoke", "quiet danger", () => confirmRemove(t("Revoke the device {name}?", { name: d.name }), t("Its next request is refused. {member}'s other devices keep working.", { member: m.name }), "device-revoke", d.id)));
      box.append(r);
    }
    const use = el("div", "rl-sub", t("Usage"));
    const holder = el("div");
    box.append(use, holder);
    holder.append(el("div", "rl-k", t("Loading…")));
    api("relay/usage?member=" + encodeURIComponent(m.id) + "&period=" + encodeURIComponent(period)).then(
      (u) => { if (holder.isConnected) holder.replaceChildren(usageBlock(u, true)); },
      (e) => { if (holder.isConnected) holder.replaceChildren(el("div", "rl-err", t(e.message))); });
    return box;
  }

  function confirmRemove(title, text, action, id) {
    const ed = editor(title);
    ed.append(el("p", "lib-confirm", text));
    const go = btn("Remove", "primary", async () => {
      go.disabled = true;
      if (await act(action, { id }, err)) closeModal(); else go.disabled = false;
    });
    if (action === "device-revoke") go.textContent = t("Revoke");
    const err = bar(ed, go);
    openModal(ed);
    $("#modal").classList.add("lib");
  }
  function removeMember(m) {
    confirmRemove(t("Remove the member {name}?", { name: m.name }), t("Their devices stop working at once and their sign-in ends. What they used stays in the ledger."), "member-remove", m.id);
  }

  async function invite(m) {
    const out = await act("invite", { id: m.id });
    if (!out) return;
    const ed = editor("Invitation for {name}");
    ed.querySelector(".ehead b").textContent = t("Invitation for {name}", { name: m.name });
    ed.append(el("p", "lib-confirm", t("Send this to {name} privately. It is shown only now, works once, and ends {when}. Whoever has it can take the account until it is used; making another cancels this one.", { name: m.name, when: when(out.expires) })));
    const link = st.page + "#invite=" + encodeURIComponent(out.code);
    const code = field(ed, "Code", el("code", "rl-secret", out.code));
    const lk = field(ed, "Link", el("code", "rl-secret", link), "Opens the hub's page with the code filled in. In Mag: Hub, Invitation code.");
    void code; void lk;
    const b = el("div", "bar");
    b.append(el("span", "grow"), btn("Copy code", "", () => copyOut(out.code)), btn("Copy link", "", () => copyOut(link)), btn("Done", "primary", () => closeModal()));
    ed.append(b);
    openModal(ed);
  }

  function editMember(m) {
    const d = { id: m?.id || "", name: m?.name || "", display: m?.display || "", off: !!m?.off, groups: [...(m?.groups || [])], plan: m?.plan || "",
      tokens: m?.limit?.tokens || "", cost: m?.limit?.cost || "", period: m?.limit?.period || "month", concurrency: m?.concurrency || "", queue: m?.queue || "" };
    const render = () => {
      const ed = editor(m ? "Edit member" : "Add member");
      const name = field(ed, "Name", input(d.name, "text", "alice"), "What they sign in as: lower-case letters, digits, dots, dashes."); name.oninput = () => { d.name = name.value; };
      const disp = field(ed, "Display name", input(d.display)); disp.oninput = () => { d.display = disp.value; };
      const gs = el("div", "rl-checks");
      if (!st.groups.length) gs.append(el("span", "rl-k", t("No groups yet")));
      for (const g of st.groups) {
        const l = el("label", "rl-check");
        const c = el("input"); c.type = "checkbox"; c.checked = d.groups.includes(g.id);
        c.onchange = () => { d.groups = c.checked ? [...d.groups, g.id] : d.groups.filter((x) => x !== g.id); };
        l.append(c, el("span", "", g.name));
        gs.append(l);
      }
      field(ed, "Groups", gs, "The models of every group ticked are theirs to use.");
      field(ed, "Plan", choice([["", "No plan"], ...st.plans.map((p) => [p.id, p.name])], d.plan, (v) => { d.plan = v; render(); }));
      const lim = el("div", "rl-inline");
      const tok = input(d.tokens, "number", "tokens"); tok.min = 0; tok.oninput = () => { d.tokens = tok.value; };
      const cost = input(d.cost, "number", "US dollars"); cost.min = 0; cost.step = "0.01"; cost.oninput = () => { d.cost = cost.value; };
      lim.append(tok, cost, choice(WINDOWS, d.period, (v) => { d.period = v; render(); }));
      field(ed, "Own limit", lim, "Tokens and estimated upstream cost for each day, week or month, in place of the plan's limit. Empty for the plan's.");
      const lanes = el("div", "rl-inline");
      const conc = input(d.concurrency, "number", "2"); conc.min = 0; conc.oninput = () => { d.concurrency = conc.value; };
      const q = input(d.queue, "number", "4"); q.min = 0; q.oninput = () => { d.queue = q.value; };
      lanes.append(conc, q);
      field(ed, "At once / waiting", lanes, "How many of their requests run at once, and how many more wait their turn. Empty for the plan's or the hub's defaults.");
      if (m) {
        const l = el("label", "rl-check");
        const c = el("input"); c.type = "checkbox"; c.checked = d.off; c.onchange = () => { d.off = c.checked; };
        l.append(c, el("span", "", t("Turned off: every device refused and sign-in ended, nothing removed")));
        field(ed, "Access", l);
      }
      const go = btn(m ? "Save" : "Add member", "primary", async () => {
        go.disabled = true;
        const limit = Number(d.tokens) > 0 || Number(d.cost) > 0 ? { period: d.period, tokens: Math.floor(Number(d.tokens) || 0), cost: Number(d.cost) || 0 } : null;
        const ok = await act("member-save", { member: { id: d.id, name: d.name.trim(), display: d.display.trim(), off: d.off, groups: d.groups, plan: d.plan, limit, concurrency: Math.floor(Number(d.concurrency) || 0), queue: Math.floor(Number(d.queue) || 0) } }, err);
        if (ok) { closeModal(); if (!m) status(t("Member added. Send them an invitation."), "ok"); } else go.disabled = false;
      });
      const err = bar(ed, go);
      openModal(ed);
    };
    render();
  }

  function editGroup(g) {
    const d = { id: g?.id || "", name: g?.name || "", models: new Set(g?.models || []) };
    const known = new Set(st.models.map((m) => m.id));
    let extra = [...d.models].filter((m) => !known.has(m)).join("\n");
    const ed = editor(g ? "Edit group" : "Add group");
    const name = field(ed, "Name", input(d.name, "text", "lab")); name.oninput = () => { d.name = name.value; };
    const list = el("div", "rl-checks tall");
    if (!st.models.length) list.append(el("span", "rl-k", t("No models yet: add a provider first.")));
    for (const m of st.models) {
      const l = el("label", "rl-check");
      const c = el("input"); c.type = "checkbox"; c.checked = d.models.has(m.id);
      c.onchange = () => { c.checked ? d.models.add(m.id) : d.models.delete(m.id); };
      l.append(c, el("span", "mono", m.id));
      if (m.group) l.append(el("span", "rl-chip", t("routing group")));
      list.append(l);
    }
    field(ed, "Models", list, "A routing group ticked lets its members use the group as one model, through every account in it.");
    const more = el("textarea", "rl-in"); more.rows = 2; more.value = extra; more.spellcheck = false; more.placeholder = "anthropic/*";
    more.oninput = () => { extra = more.value; };
    field(ed, "Patterns", more, "One a line: provider/* for every model of a provider, group/* for every routing group.");
    const go = btn(g ? "Save" : "Add group", "primary", async () => {
      go.disabled = true;
      const models = [...[...d.models].filter((m) => known.has(m)), ...extra.split(/\n/).map((s) => s.trim()).filter(Boolean)];
      if (await act("group-save", { group: { id: d.id, name: d.name.trim(), models } }, err)) closeModal(); else go.disabled = false;
    });
    const err = bar(ed, go);
    openModal(ed);
  }

  function editPlan(p) {
    const d = { id: p?.id || "", name: p?.name || "", kind: p?.kind || "period", window: p?.window || "month", priceIn: p?.priceIn || "", priceOut: p?.priceOut || "",
      priceCacheRead: p?.priceCacheRead || "", priceCacheWrite: p?.priceCacheWrite || "", budget: p?.budget || "", price: p?.price || "", tokens: p?.tokens || "",
      cacheReads: !!p?.cacheReads, concurrency: p?.concurrency || "", queue: p?.queue || "" };
    const numIn = (key, ph, step) => { const i = input(d[key], "number", ph); i.min = 0; if (step) i.step = step; i.oninput = () => { d[key] = i.value; }; return i; };
    const render = () => {
      const ed = editor(p ? "Edit plan" : "Add plan");
      const name = field(ed, "Name", input(d.name)); name.oninput = () => { d.name = name.value; };
      field(ed, "Kind", choice([["period", "Per period, tokens included"], ["metered", "Metered, by the token"]], d.kind, (v) => { d.kind = v; render(); }));
      field(ed, "Window", choice(WINDOWS, d.window, (v) => { d.window = v; render(); }), "The calendar window its limit counts in and resets by.");
      if (d.kind === "period") {
        field(ed, "Price", numIn("price", "US dollars", "0.01"), "For each window. It is shown on statements; nothing is collected.");
        field(ed, "Tokens included", numIn("tokens", "0 for no limit"), "Uncached input, output and cache writes. Used up, requests are refused until the window resets.");
        const l = el("label", "rl-check");
        const c = el("input"); c.type = "checkbox"; c.checked = d.cacheReads; c.onchange = () => { d.cacheReads = c.checked; };
        l.append(c, el("span", "", t("Count cache reads among them")));
        field(ed, "Cache", l);
      } else {
        const pr = el("div", "rl-inline");
        pr.append(numIn("priceIn", "input", "0.01"), numIn("priceOut", "output", "0.01"), numIn("priceCacheRead", "cache read", "0.01"), numIn("priceCacheWrite", "cache write", "0.01"));
        field(ed, "Prices", pr, "US dollars for a million tokens: input, output, cache reads, cache writes.");
        field(ed, "Budget", numIn("budget", "0 for no cap", "0.01"), "The most the plan charges in a window; past it requests are refused until it resets.");
      }
      const lanes = el("div", "rl-inline");
      lanes.append(numIn("concurrency", "2"), numIn("queue", "4"));
      field(ed, "At once / waiting", lanes, "Its members' requests running at once, and waiting. Empty for the hub's defaults.");
      const go = btn(p ? "Save" : "Add plan", "primary", async () => {
        go.disabled = true;
        const n = (k) => Number(d[k]) || 0;
        const plan = { id: d.id, name: d.name.trim(), kind: d.kind, window: d.window, priceIn: n("priceIn"), priceOut: n("priceOut"), priceCacheRead: n("priceCacheRead"), priceCacheWrite: n("priceCacheWrite"),
          budget: n("budget"), price: n("price"), tokens: Math.floor(n("tokens")), cacheReads: d.cacheReads, concurrency: Math.floor(n("concurrency")), queue: Math.floor(n("queue")) };
        if (await act("plan-save", { plan }, err)) closeModal(); else go.disabled = false;
      });
      const err = bar(ed, go);
      openModal(ed);
    };
    render();
  }
  if (view === "members") load();
})();
