// Preset editor: drag to reorder, −/+ sets, remove, and search-to-add.
//
// The form posts the list in its on-screen order (one hidden "item" and one
// hidden "sets" input per row), so all this script does is move, add and
// remove <li> rows. The server works out positions from the order.
(() => {
  const list = document.getElementById("items");
  const empty = document.getElementById("items-empty");
  const search = document.getElementById("exercise-search");
  const results = document.getElementById("search-results");
  const template = document.getElementById("item-template");
  const allExercises = JSON.parse(document.getElementById("all-exercises").textContent) || [];

  // Drag & drop. Only the ⠿ handle starts a drag, so swiping anywhere else
  // still scrolls the page on a phone.
  // forceFallback makes SortableJS track the pointer itself instead of using
  // the browser's built-in drag & drop, so mouse and touch behave the same.
  Sortable.create(list, {
    handle: ".handle",
    animation: 150,
    ghostClass: "dragging",
    forceFallback: true,
    fallbackClass: "drag-ghost",
  });

  // −/+ and ✕ buttons, handled once for the whole list (works for new rows too).
  list.addEventListener("click", (event) => {
    const button = event.target.closest("button");
    if (!button) return;
    const row = button.closest(".item");

    if (button.classList.contains("remove")) {
      row.remove();
      refresh();
      return;
    }
    if (button.dataset.step) {
      const input = row.querySelector('input[name="sets"]');
      const sets = Math.min(10, Math.max(1, Number(input.value) + Number(button.dataset.step)));
      input.value = sets;
      row.querySelector(".sets-label b").textContent = sets;
    }
  });

  // Values ("12" or "new:name") already in the list, lower-cased.
  function inList() {
    return new Set([...list.querySelectorAll('input[name="item"]')].map((i) => i.value.toLowerCase()));
  }

  function addRow(value, name) {
    const row = template.content.firstElementChild.cloneNode(true);
    row.querySelector('input[name="item"]').value = value;
    row.querySelector(".item-name").textContent = name;
    row.querySelector(".remove").setAttribute("aria-label", "Remove " + name);
    if (value.startsWith("new:")) {
      row.querySelector(".item-name").insertAdjacentHTML("beforeend", ' <small class="badge-new">new</small>');
    }
    list.appendChild(row);
    search.value = "";
    refresh();
    search.focus(); // ready to add the next one
  }

  // Redraw the search results and the "no exercises" note.
  function refresh() {
    empty.hidden = list.children.length > 0;
    results.replaceChildren();

    const query = search.value.trim();
    if (!query) return;
    const taken = inList();

    // Substring matches and near-misses ("bnech" finds "Bench press").
    const matches = allExercises
      .filter((e) => Similar.matches(e.name, query) && !taken.has(String(e.id)))
      .slice(0, 8);
    for (const e of matches) {
      results.appendChild(resultButton(e.name, "+ Add", () => addRow(String(e.id), e.name)));
    }

    // Offer to create it, unless it already exists (ignoring case, accents,
    // punctuation). If it merely looks like a typo of one, say "anyway".
    const exists =
      allExercises.some((e) => Similar.isSame(e.name, query)) ||
      [...taken].some((v) => v.startsWith("new:") && Similar.isSame(v.slice(4), query));
    if (!exists) {
      const typo = allExercises.some((e) => Similar.isSimilar(e.name, query));
      const label = typo ? `Create “${query}” anyway` : `Create “${query}”`;
      results.appendChild(resultButton(label, "+ New", () => addRow("new:" + query, query)));
    }
  }

  function resultButton(label, action, onPick) {
    const li = document.createElement("li");
    const button = document.createElement("button");
    button.type = "button";
    button.innerHTML = "<span></span><small></small>";
    button.querySelector("span").textContent = label;
    button.querySelector("small").textContent = action;
    button.addEventListener("click", onPick);
    li.appendChild(button);
    return li;
  }

  search.addEventListener("input", refresh);

  // Enter adds the first result instead of submitting the whole form.
  search.addEventListener("keydown", (event) => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    results.querySelector("button")?.click();
  });
})();
