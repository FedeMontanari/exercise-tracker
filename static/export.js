// CSV export that doesn't get stuck in an iPhone home-screen web app.
//
// A home-screen app has no back button, so following a link to a .csv
// replaces the whole app with a file preview you can't leave. When running
// as a home-screen app we fetch the CSV ourselves and hand it to the iOS
// share sheet (Save to Files, AirDrop, Mail, Numbers…) instead. Everywhere
// else the links work as normal downloads, and this script does nothing.
(() => {
  const standalone =
    navigator.standalone === true || window.matchMedia("(display-mode: standalone)").matches;
  if (!standalone) return;

  const status = document.getElementById("export-status");

  function show(message) {
    status.textContent = message;
    status.hidden = !message;
  }

  // The server names the file in its Content-Disposition header.
  function filenameFrom(response) {
    const match = /filename="([^"]+)"/.exec(response.headers.get("Content-Disposition") || "");
    return match ? match[1] : "sessions.csv";
  }

  async function exportCSV(url) {
    show("Preparing file…");
    let file;
    try {
      const response = await fetch(url);
      if (!response.ok) throw new Error(await response.text());
      file = new File([await response.blob()], filenameFrom(response), { type: "text/csv" });
    } catch (err) {
      show("Couldn't create the file: " + err.message);
      return;
    }

    if (!navigator.canShare || !navigator.canShare({ files: [file] })) {
      // No share sheet: open in a separate window, which has a "Done" button.
      show("");
      window.open(url, "_blank");
      return;
    }

    try {
      await navigator.share({ files: [file] });
      show("");
    } catch (err) {
      if (err.name === "AbortError") {
        show(""); // you closed the share sheet
      } else if (err.name === "NotAllowedError") {
        // Safari only allows sharing straight after a tap; if preparing the
        // file took too long, ask for one more tap.
        offerShareButton(file);
      } else {
        show("Couldn't share the file: " + err.message);
      }
    }
  }

  function offerShareButton(file) {
    show("");
    const button = document.createElement("button");
    button.type = "button";
    button.className = "primary";
    button.textContent = `Save ${file.name}`;
    button.addEventListener("click", async () => {
      try {
        await navigator.share({ files: [file] });
      } catch (_) {
        /* closed or failed; the button stays so you can retry */
      }
    });
    status.replaceChildren(button);
    status.hidden = false;
  }

  // Quick-range links.
  document.querySelectorAll("a[data-export]").forEach((link) => {
    link.addEventListener("click", (event) => {
      event.preventDefault();
      exportCSV(link.href);
    });
  });

  // The From/To form: build the same URL a normal GET submit would.
  const form = document.getElementById("export-form");
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    const params = new URLSearchParams(new FormData(form));
    exportCSV(form.action + "?" + params);
  });
})();
