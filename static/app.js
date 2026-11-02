// The Photo Booth front end: camera or file in, strip out.
// Everything the server does happens in POST /api/strip; this file only stages it.
(() => {
  const $ = (id) => document.getElementById(id);
  const booth = $("booth"), cabin = $("cabin"), video = $("video"), preview = $("preview");
  const idle = $("glassIdle"), count = $("count"), pose = $("pose"), status = $("status");
  const shoot = $("shoot"), file = $("file"), modeCam = $("modeCam"), modePhoto = $("modePhoto");
  const stripImg = $("stripImg"), tag = $("stripTag"), download = $("download"), flash = $("flash");
  const lamps = [...document.querySelectorAll(".lamps li")];
  const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const MAX = 8 * 1024 * 1024;
  const TYPES = ["image/jpeg", "image/png", "image/webp", "image/gif"];
  let mode = null, stream = null, photo = null, busy = false, stripUrl = null;

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const kb = (n) => Math.round(n / 1024) + " KB";
  function say(msg, kind) { status.textContent = msg; status.dataset.kind = kind || ""; }
  function choose(m) {
    mode = m;
    modeCam.setAttribute("aria-pressed", String(m === "camera"));
    modePhoto.setAttribute("aria-pressed", String(m === "photo"));
    idle.hidden = true;
    cabin.dataset.state = "open";
    shoot.disabled = false;
  }
  function stopCamera() {
    if (stream) stream.getTracks().forEach((t) => t.stop());
    stream = null;
    video.srcObject = null;
    video.hidden = true;
  }
  function lock(on) {
    busy = on;
    [shoot, modeCam, modePhoto].forEach((b) => (b.disabled = on));
    if (!on) shoot.disabled = !mode;
  }

  // 1. Pick a source
  modeCam.addEventListener("click", async () => {
    if (busy) return;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      say("This browser cannot open a camera on this page. Choose a photo instead.", "error");
      return;
    }
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: "user", width: { ideal: 1280 }, height: { ideal: 960 } }, audio: false,
      });
    } catch {
      say("The camera is blocked or missing. Allow camera access, or choose a photo instead.", "error");
      return;
    }
    photo = null;
    preview.hidden = true;
    video.srcObject = stream;
    video.hidden = false;
    await video.play().catch(() => {});
    choose("camera");
    say("Camera on. Insert the coin, then hold still for four flashes.");
  });

  modePhoto.addEventListener("click", () => { if (!busy) file.click(); });
  file.addEventListener("change", () => {
    const f = file.files[0];
    file.value = "";
    if (!f) return;
    if (!TYPES.includes(f.type)) {
      say("This file is not a photo the booth can print. Use JPEG, PNG, WebP or GIF.", "error");
      return;
    }
    if (f.size > MAX) {
      say("This photo weighs " + (f.size / 1048576).toFixed(1) + " MB. The booth takes 8 MB at most.", "error");
      return;
    }
    stopCamera();
    photo = f;
    if (preview.src.startsWith("blob:")) URL.revokeObjectURL(preview.src);
    preview.src = URL.createObjectURL(f);
    preview.hidden = false;
    choose("photo");
    say("Photo loaded. Insert the coin to print it.");
  });

  // 2. Insert the coin: countdown, flashes, then develop on the server
  shoot.addEventListener("click", async () => {
    if (busy || !mode) return;
    lock(true);
    shoot.classList.remove("is-paid");
    void shoot.offsetWidth;
    shoot.classList.add("is-paid");
    try {
      const shots = [];
      if (mode === "camera") {
        for (let i = 1; i <= 4; i++) {
          pose.textContent = "Pose " + i + " of 4";
          await countdown(i === 1 ? 3 : 2);
          shots.push(await grab());
          await fire();
        }
      } else {
        pose.textContent = "One photo, four frames";
        await countdown(3);
        await fire();
        shots.push(photo);
      }
      pose.textContent = "";
      await develop(shots);
    } catch (e) {
      say(e.message || "Something went wrong. Try again.", "error");
      lamps.forEach((l) => l.classList.remove("is-lit"));
    } finally {
      pose.textContent = "";
      count.textContent = "";
      cabin.classList.remove("is-counting");
      booth.classList.remove("is-developing");
      lock(false);
    }
  });

  async function countdown(n) {
    cabin.classList.add("is-counting");
    for (let k = n; k >= 1; k--) {
      count.textContent = String(k);
      count.classList.remove("tick");
      void count.offsetWidth;
      count.classList.add("tick");
      await sleep(800);
    }
    count.textContent = "";
    cabin.classList.remove("is-counting");
  }

  async function fire() {
    flash.classList.remove("go");
    void flash.offsetWidth;
    flash.classList.add("go");
    await sleep(reduce ? 120 : 320);
  }

  // Capture the current camera frame, mirrored like the preview, as a JPEG.
  function grab() {
    const w = Math.min(video.videoWidth || 1280, 1280);
    const h = Math.round(w * ((video.videoHeight || 960) / (video.videoWidth || 1280)));
    const c = document.createElement("canvas");
    c.width = w;
    c.height = h;
    const ctx = c.getContext("2d");
    ctx.translate(w, 0);
    ctx.scale(-1, 1);
    ctx.drawImage(video, 0, 0, w, h);
    return new Promise((resolve, reject) =>
      c.toBlob((b) => (b ? resolve(b) : reject(new Error("The camera frame could not be captured."))), "image/jpeg", 0.9));
  }

  async function develop(shots) {
    booth.classList.add("is-developing");
    say("Developing. ImageMagick is printing your strip on the server.");
    lamps.forEach((l) => l.classList.remove("is-lit"));
    let lit = 0;
    const timer = setInterval(() => { if (lit < 3) lamps[lit++].classList.add("is-lit"); }, 380);

    const form = new FormData();
    shots.forEach((b, i) => form.append("photo", b, "pose-" + (i + 1) + extension(b.type)));
    let res;
    try {
      res = await fetch("/api/strip", { method: "POST", body: form });
    } catch {
      clearInterval(timer);
      throw new Error("The booth could not be reached. Check the connection and try again.");
    }
    clearInterval(timer);
    if (!res.ok) {
      let msg = "The strip could not be printed (HTTP " + res.status + ").";
      try { msg = (await res.json()).error || msg; } catch {}
      throw new Error(msg);
    }
    const blob = await res.blob();
    lamps.forEach((l) => l.classList.add("is-lit"));
    deliver(blob, res.headers);
  }

  function extension(type) {
    return { "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif" }[type] || ".jpg";
  }

  // 3. The strip drops in the chute, and the darkroom shows how it was made.
  function deliver(blob, h) {
    if (stripUrl) URL.revokeObjectURL(stripUrl);
    stripUrl = URL.createObjectURL(blob);
    const by = h.get("X-Developed-By") || "the server";
    const ms = h.get("X-Magick-Ms") || "?";
    stripImg.hidden = false;
    stripImg.alt = "Your photo strip, in sepia, halftone, posterize and film grain.";
    stripImg.classList.remove("is-out");
    stripImg.src = stripUrl;
    void stripImg.offsetWidth;
    stripImg.classList.add("is-out");
    download.href = stripUrl;
    download.download = "photo-strip.jpg";
    download.hidden = false;
    tag.textContent = "Your strip, printed by instance " + by + " in " + ms + " ms. The server kept no copy.";
    say("Your strip is ready. Download it from the chute, or insert the coin again.");

    const cmd = h.get("X-Magick-Command");
    if (cmd) renderCommand(decode(cmd));
    $("statIn").textContent = (h.get("X-Input-Count") || "?") + " · " + kb(+h.get("X-Input-Bytes") || 0);
    $("statMs").textContent = ms + " ms";
    $("statOut").textContent = kb(blob.size) + " JPEG";
    $("statBy").textContent = "instance " + by + ", just now";
    if (matchMedia("(max-width: 720px)").matches) {
      stripImg.scrollIntoView({ behavior: reduce ? "auto" : "smooth", block: "center" });
    }
  }

  function decode(b64) {
    const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  }

  // Same rendering as the server template: one line per frame, colored by recipe.
  function renderCommand(text) {
    const code = document.querySelector("#cmd code");
    code.textContent = "";
    let frame = 0, current = 0;
    text.split("\n").forEach((line) => {
      const span = document.createElement("span");
      span.className = "cmd__line";
      if (line.trim().startsWith("\\(")) current = ++frame;
      else if (!line.startsWith("     ")) current = 0; // a frame's recipe line is indented
      if (current) span.classList.add("f" + current);
      span.textContent = line;
      code.append(span, "\n");
    });
    const pre = $("cmd");
    pre.classList.remove("is-new");
    void pre.offsetWidth;
    pre.classList.add("is-new");
  }
})();
