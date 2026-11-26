// Exercises page: one box that searches the list and adds new exercises,
// with warnings for duplicates and likely typos (see similar.js).
(() => {
  const search = document.getElementById("ex-search");
  const hint = document.getElementById("ex-hint");
  const addButton = document.getElementById("ex-add");
  const none = document.getElementById("ex-none");
  const items = [...document.querySelectorAll(".ex-item")];
  const all = items.map((li) => ({ id: li.dataset.id, name: li.dataset.name, li }));

  function setHint(text, kind) {
    hint.textContent = text;
    hint.className = "ex-hint " + (kind || "");
    hint.hidden = !text;
  }

  // Filter the list and decide whether (and how) to offer "Add".
  function refresh() {
    const query = search.value.trim();

    let shown = 0;
    for (const ex of all) {
      const visible = Similar.matches(ex.name, query);
      ex.li.hidden = !visible;
      if (visible) shown++;
    }
    none.hidden = shown > 0;

    if (!query) {
      setHint("");
      addButton.hidden = true;
      return;
    }

    const same = all.find((ex) => Similar.isSame(ex.name, query));
    if (same) {
      setHint(`“${same.name}” already exists.`, "warn");
      addButton.hidden = true;
      return;
    }

    const similar = all.filter((ex) => Similar.isSimilar(ex.name, query)).map((ex) => `“${ex.name}”`);
    if (similar.length) {
      setHint(`Looks like ${similar.join(", ")}. Typo?`, "warn");
      addButton.textContent = `+ Add “${query}” anyway`;
    } else {
      setHint("");
      addButton.textContent = `+ Add “${query}”`;
    }
    addButton.hidden = false;
  }

  search.addEventListener("input", refresh);

  // Enter adds, unless that exact exercise already exists: then open it.
  search.addEventListener("keydown", (event) => {
    if (event.key !== "Enter") return;
    const same = all.find((ex) => Similar.isSame(ex.name, search.value.trim()));
    if (same || !search.value.trim()) {
      event.preventDefault();
      if (same) {
        same.li.querySelector("details").open = true;
        same.li.scrollIntoView({ block: "center" });
      }
    }
  });

  // Fill each "Merge into" list when its panel first opens: likely
  // duplicates first, then everything else A–Z.
  for (const ex of all) {
    const details = ex.li.querySelector("details");
    details.addEventListener("toggle", () => {
      const select = details.querySelector("select[name=into]");
      if (!details.open || select.options.length > 1) return;

      const others = all.filter((o) => o.id !== ex.id);
      const likely = others.filter((o) => Similar.isSimilar(o.name, ex.name));
      const rest = others.filter((o) => !likely.includes(o));
      const group = (label, list) => {
        if (!list.length) return;
        const og = document.createElement("optgroup");
        og.label = label;
        for (const o of list) og.appendChild(new Option(o.name, o.id));
        select.appendChild(og);
      };
      group("Likely duplicates", likely);
      group("All exercises", rest);
    });
  }

  // "Add several": flag lines that already exist or look like typos.
  const bulk = document.getElementById("bulk-names");
  const bulkWarnings = document.getElementById("bulk-warnings");
  bulk.addEventListener("input", () => {
    bulkWarnings.replaceChildren();
    for (const line of bulk.value.split("\n").map((l) => l.trim()).filter(Boolean)) {
      const same = all.find((ex) => Similar.isSame(ex.name, line));
      const similar = !same && all.find((ex) => Similar.isSimilar(ex.name, line));
      if (!same && !similar) continue;
      const li = document.createElement("li");
      li.textContent = same
        ? `“${line}” already exists — it will be skipped.`
        : `“${line}” looks like “${similar.name}”. Typo?`;
      bulkWarnings.appendChild(li);
    }
  });

  if (search.value) refresh();
})();
