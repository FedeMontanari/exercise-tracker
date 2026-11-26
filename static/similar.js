// Spotting likely duplicate exercise names, e.g.
//   "Bench press" ~ "bench-press" ~ "Bench  Press" ~ "Bnech press" ~ "Bench presses"
//   "Elevación lateral" ~ "Elevacion lateral"
// Used by the Exercises page and the preset editor's search.
window.Similar = (() => {
  // Lower-case, strip accents, keep only letters and digits.
  function normalize(name) {
    return name
      .toLowerCase()
      .normalize("NFD")
      .replace(/[̀-ͯ]/g, "") // accents: "ó" → "o"
      .replace(/[^a-z0-9]/g, "");
  }

  // Edit distance: how many typos turn a into b, where a typo is one
  // letter added, removed, changed, or two neighbours swapped ("bnech" →
  // "bench" is one). Dynamic programming: d[i][j] is the distance between
  // the first i letters of a and the first j letters of b.
  function distance(a, b) {
    const d = Array.from({ length: a.length + 1 }, (_, i) => [i]);
    for (let j = 1; j <= b.length; j++) d[0][j] = j;
    for (let i = 1; i <= a.length; i++) {
      for (let j = 1; j <= b.length; j++) {
        const cost = a[i - 1] === b[j - 1] ? 0 : 1;
        d[i][j] = Math.min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + cost);
        if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1]) {
          d[i][j] = Math.min(d[i][j], d[i - 2][j - 2] + 1); // swapped neighbours
        }
      }
    }
    return d[a.length][b.length];
  }

  // Same name written differently, or a typo or two apart (plurals count as
  // the same). Short names allow one typo, longer ones two.
  function isSimilar(a, b) {
    const x = normalize(a);
    const y = normalize(b);
    if (!x || !y) return false;
    const singular = (s) => s.replace(/s$/, "");
    const typos = Math.min(distance(x, y), distance(singular(x), singular(y)));
    return typos <= (Math.min(x.length, y.length) <= 5 ? 1 : 2);
  }

  // Same name once normalized: "Bench Press" and "bench-press" are one exercise.
  function isSame(a, b) {
    return normalize(a) === normalize(b);
  }

  // Does name match what's typed in a search box (substring or near-miss)?
  function matches(name, query) {
    const q = normalize(query);
    return !q || normalize(name).includes(q) || isSimilar(name, query);
  }

  return { normalize, isSimilar, isSame, matches };
})();
