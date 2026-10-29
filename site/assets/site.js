// The shinyctl site's only script: the terminal on the landing page, and copy
// buttons. Every page reads fine without it.
(() => {
  "use strict";
  document.documentElement.classList.remove("no-js");

  let reduce = false;
  try { reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches; } catch (e) { /* old browser */ }

  // copyText puts text on the clipboard; if the browser refuses, it selects
  // node's text so the reader can copy it by hand.
  function copyText(text, node, button) {
    const label = button.textContent;
    const done = () => { button.textContent = "Copied"; setTimeout(() => (button.textContent = label), 1600); };
    const fallback = () => {
      const r = document.createRange();
      r.selectNodeContents(node);
      const s = window.getSelection();
      s.removeAllRanges();
      s.addRange(r);
    };
    try { navigator.clipboard.writeText(text).then(done, fallback); } catch (e) { fallback(); }
  }

  document.querySelectorAll("[data-copy]").forEach((button) => {
    const node = document.getElementById(button.dataset.copy);
    if (node) button.addEventListener("click", () => copyText(node.dataset.text || node.textContent, node, button));
  });

  document.querySelectorAll(".prose pre").forEach((pre) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "copy-code";
    button.textContent = "Copy";
    button.addEventListener("click", () => copyText(pre.querySelector("code").textContent, pre, button));
    pre.appendChild(button);
  });

  const term = document.getElementById("term");
  if (!term) return;

  // Real output of shinyctl v0.1.0 running examples/hello. A line that starts
  // with "§" is drawn highlighted: it is what changed.
  const scenes = [
    {
      caption: "One binary, checked against the release's checksums, copied into ~/.local/bin. Nothing needs root.",
      steps: [
        ["cmd", "curl -fsSL https://raw.githubusercontent.com/spriyansh/shinyctl/main/install.sh | sh"],
        ["out", ["Downloading shinyctl v0.1.0 for darwin/arm64", "Installed shinyctl v0.1.0 to /Users/you/.local/bin/shinyctl", ""]],
        ["cmd", "shinyctl version"],
        ["out", ["v0.1.0"]],
      ],
    },
    {
      caption: "apply starts shinyctl-api and two R processes behind port 8765: copy 1 takes visitors, copy 2 waits as a warm spare.",
      steps: [
        ["cmd", "shinyctl apply -f examples/hello/shinyctl.yaml"],
        ["out", ["Starting app \"hello\" on port 8765 (1 copy and 1 warm spare)", ""]],
        ["cmd", "shinyctl status"],
        ["out", [
          "NAME   COPY  ROLE  PORT  PID    STATUS  READY     MEM  SESSIONS  CRASHES  STARTED",
          "hello  1     -     8765  77713  alive   starting  96M  0         -        2026-10-29T22:05:11+01:00",
          "hello  2     -     8765  77714  alive   starting  96M  0         -        2026-10-29T22:05:11+01:00",
          "",
          "Memory: 192M in 2 R processes, plus 37M for the load balancer.",
          "",
        ]],
        ["gap", "a few seconds later"],
        ["cmd", "shinyctl status"],
        ["out", [
          "NAME   COPY  ROLE    PORT  PID    STATUS  READY  MEM  SESSIONS  CRASHES  STARTED",
          "§hello  1     active  8765  77713  alive   ready  97M  0         -        2026-10-29T22:05:11+01:00",
          "§hello  2     warm    8765  77714  alive   ready  97M  0         -        2026-10-29T22:05:11+01:00",
          "",
          "Memory: 195M in 2 R processes (1 warm), plus 37M for the load balancer.",
          "",
        ]],
        ["cmd", "curl -sI http://127.0.0.1:8765 | head -1"],
        ["out", ["HTTP/1.1 200 OK"]],
      ],
    },
    {
      caption: "The active copy is killed. The spare answers the very next request, and the dead copy comes back as the new spare.",
      steps: [
        ["cmd", "kill -9 77713"],
        ["cmd", "curl -sI http://127.0.0.1:8765 | head -1"],
        ["out", ["§HTTP/1.1 200 OK", ""]],
        ["cmd", "shinyctl status"],
        ["out", [
          "NAME   COPY  ROLE    PORT  PID    STATUS         READY  MEM   SESSIONS  CRASHES  STARTED",
          "§hello  1     -       8765  77713  crash-looping  -      -     -         1        2026-10-29T22:05:11+01:00",
          "§hello  2     active  8765  77714  alive          ready  109M  0         -        2026-10-29T22:05:11+01:00",
          "",
          "Memory: 109M in 1 R process, plus 38M for the load balancer.",
          "",
        ]],
        ["gap", "a few seconds later"],
        ["cmd", "shinyctl status"],
        ["out", [
          "NAME   COPY  ROLE    PORT  PID    STATUS  READY  MEM   SESSIONS  CRASHES  STARTED",
          "§hello  1     warm    8765  77839  alive   ready  97M   0         1        2026-10-29T22:05:16+01:00",
          "hello  2     active  8765  77714  alive   ready  109M  0         -        2026-10-29T22:05:11+01:00",
          "",
          "Memory: 206M in 2 R processes (1 warm), plus 38M for the load balancer.",
        ]],
      ],
    },
    {
      caption: "restart starts the new version first. The old copy drains: it keeps its visitors until they leave, then stops.",
      steps: [
        ["cmd", "shinyctl restart -f examples/hello/shinyctl.yaml"],
        ["out", ["Restarting app \"hello\": starting copy 3; copies 1 and 2 keep serving until the new one is ready, then drain for up to 15m", ""]],
        ["cmd", "shinyctl status"],
        ["out", [
          "NAME   COPY  ROLE      PORT  PID    STATUS  READY     MEM   SESSIONS  CRASHES  STARTED",
          "§hello  2     draining  8765  77714  alive   ready     109M  0         -        2026-10-29T22:05:11+01:00",
          "§hello  3     active    8765  77886  alive   ready     97M   0         -        2026-10-29T22:05:28+01:00",
          "hello  4     -         8765  77905  alive   starting  98M   0         -        2026-10-29T22:05:29+01:00",
          "",
          "Memory: 304M in 3 R processes (1 draining), plus 38M for the load balancer.",
          "",
          "hello: copy 4, the new spare, is starting; until it is ready the app has no spare.",
          "",
          "hello: copy 2 is draining with no sessions; it stops within seconds.",
          "",
        ]],
        ["gap", "a few seconds later"],
        ["cmd", "shinyctl status"],
        ["out", [
          "NAME   COPY  ROLE    PORT  PID    STATUS  READY  MEM   SESSIONS  CRASHES  STARTED",
          "hello  3     active  8765  77886  alive   ready  100M  0         -        2026-10-29T22:05:28+01:00",
          "§hello  4     warm    8765  77905  alive   ready  101M  0         -        2026-10-29T22:05:29+01:00",
          "",
          "Memory: 201M in 2 R processes (1 warm), plus 39M for the load balancer.",
        ]],
      ],
    },
  ];

  const caption = document.getElementById("caption");
  const tabs = Array.from(document.querySelectorAll(".scenes button"));
  const playBtn = document.getElementById("play");

  let run = 0;        // bumped to cancel whatever is playing
  let current = 0;
  let playing = !reduce;

  const stop = () => { run++; };
  const sleep = (ms, id) => new Promise((resolve, reject) =>
    setTimeout(() => (id === run ? resolve() : reject(new Error("stopped"))), ms));

  function add(cls) {
    const d = document.createElement("div");
    d.className = cls ? "ln " + cls : "ln";
    term.appendChild(d);
    term.scrollTop = term.scrollHeight;
    return d;
  }
  function outLine(text) {
    const hl = text.startsWith("§");
    add(hl ? "hl" : "").textContent = (hl ? text.slice(1) : text) || " ";
  }
  function promptLine(text) {
    const d = add("");
    const ps = document.createElement("span");
    ps.className = "ps";
    ps.textContent = "~ $ ";
    const cmd = document.createElement("span");
    cmd.className = "cmd";
    cmd.textContent = text;
    d.append(ps, cmd);
    return cmd;
  }
  function gapLine(text) { add("gap").textContent = "· " + text + " ·"; }

  function select(i) {
    current = i;
    tabs.forEach((t, k) => t.setAttribute("aria-pressed", String(k === i)));
    caption.textContent = scenes[i].caption;
  }

  function drawAll(i) {
    select(i);
    term.textContent = "";
    for (const [kind, v] of scenes[i].steps) {
      if (kind === "cmd") promptLine(v);
      else if (kind === "out") v.forEach(outLine);
      else gapLine(v);
    }
    term.scrollTop = 0;
  }

  async function animate(i, id) {
    select(i);
    term.textContent = "";
    for (const [kind, v] of scenes[i].steps) {
      if (kind === "cmd") {
        const cmd = promptLine("");
        const cursor = document.createElement("span");
        cursor.className = "cursor";
        cmd.after(cursor);
        await sleep(380, id);
        for (const ch of v) {
          cmd.textContent += ch;
          await sleep(v.length > 60 ? 14 : 34, id);
        }
        await sleep(260, id);
        cursor.remove();
      } else if (kind === "out") {
        for (const line of v) { outLine(line); await sleep(45, id); }
        await sleep(500, id);
      } else {
        gapLine(v);
        await sleep(1100, id);
      }
    }
  }

  async function playFrom(i) {
    stop();
    const id = run;
    try {
      for (let k = i; ; k = (k + 1) % scenes.length) {
        await animate(k, id);
        await sleep(3400, id);
      }
    } catch (e) { /* stopped */ }
  }

  function setPlaying(on) {
    playing = on;
    playBtn.textContent = on ? "Pause" : "Play";
    playBtn.setAttribute("aria-pressed", String(!on));
  }

  tabs.forEach((t, k) => t.addEventListener("click", () => {
    if (playing) playFrom(k);
    else { stop(); drawAll(k); }
  }));
  playBtn.addEventListener("click", () => {
    if (playing) { stop(); setPlaying(false); drawAll(current); }
    else { setPlaying(true); playFrom(current); }
  });

  // At rest the first scene is complete; the session starts playing after a moment.
  drawAll(0);
  setPlaying(playing);
  if (playing) {
    const id = run;
    setTimeout(() => { if (id === run && playing) playFrom(1); }, 2600);
  }
})();
