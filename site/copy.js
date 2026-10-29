// The install command is the one thing on the page people copy, and it is
// long enough that selecting it by hand on a phone misses a character.
document.querySelectorAll("[data-copy]").forEach((box) => {
  const btn = box.querySelector(".copy");
  const text = box.querySelector("code").textContent.trim();
  btn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(text);
      btn.textContent = "Copied";
      btn.classList.add("done");
    } catch {
      // No clipboard (an insecure context, a refused permission): select
      // the command so a long-press or Ctrl+C still works.
      const range = document.createRange();
      range.selectNodeContents(box.querySelector("code"));
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      btn.textContent = "Selected";
    }
    setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("done"); }, 1800);
  });
});
