// Random names for a new site, made of words a person can say on the
// phone: "maple-otter-comet" is dictated once, "x7k2q9" letter by letter.
// The words are short (3 to 7 letters, a to z only) so that the longest
// name offered, MAX_WORDS of them with a hyphen between, still fits in
// one DNS label of 63 characters; a longer label is refused by every
// resolver and the site would only fail when someone tried to reach it.
// The panel's tests read this list and check exactly that.

export const MAX_WORDS = 8;

const WORDS = [
  "acorn", "actor", "adobe", "agent", "album", "alpine", "amber", "anchor", "angle", "apple", "apricot", "arcade",
  "arch", "arena", "argon", "arrow", "aspen", "atlas", "atom", "aurora", "autumn", "avenue", "badge", "bagel",
  "bakery", "balcony", "bamboo", "banana", "banjo", "banner", "barley", "barn", "basil", "basket", "beach", "beacon",
  "bean", "beaver", "berry", "bicycle", "birch", "biscuit", "bison", "blanket", "blossom", "blue", "bonfire", "bongo",
  "book", "border", "bottle", "boulder", "bramble", "branch", "brave", "breeze", "brick", "bridge", "brook", "bubble",
  "bucket", "buffalo", "bugle", "bundle", "butter", "button", "cabin", "cactus", "camel", "camera", "canal", "candle",
  "candy", "canoe", "canyon", "captain", "caramel", "carbon", "cargo", "carpet", "carrot", "castle", "cedar", "cello",
  "cherry", "chess", "chili", "cider", "cinema", "circus", "citrus", "clay", "clever", "cliff", "clock", "cloud",
  "clover", "cobalt", "cocoa", "coconut", "comet", "compass", "copper", "coral", "cotton", "cougar", "cousin", "coyote",
  "cradle", "crane", "crater", "crayon", "cricket", "crisp", "crown", "crystal", "cuckoo", "cupcake", "curious", "cycle",
  "dahlia", "daisy", "dancer", "dawn", "delta", "denim", "desert", "dinner", "dolphin", "domino", "donkey", "dragon",
  "dream", "drift", "drum", "dune", "eagle", "echo", "eclipse", "ember", "emerald", "engine", "falcon", "feather",
  "fennel", "fern", "fiddle", "fiesta", "fig", "finch", "fjord", "flame", "flint", "flower", "fluffy", "flute",
  "forest", "fossil", "fox", "frost", "galaxy", "garden", "garlic", "gazelle", "gecko", "gentle", "geyser", "ginger",
  "glacier", "globe", "golden", "gopher", "gorilla", "granite", "grape", "gravel", "guitar", "gully", "hamlet", "harbor",
  "harp", "harvest", "hazel", "helium", "heron", "hickory", "hill", "honey", "horizon", "hornet", "humble", "icicle",
  "igloo", "indigo", "island", "ivory", "jacket", "jade", "jaguar", "jasmine", "jelly", "jester", "jigsaw", "jolly",
  "jungle", "juniper", "kayak", "kernel", "kettle", "kiwi", "koala", "ladder", "lagoon", "lantern", "lava", "lemon",
  "lily", "lime", "linen", "lion", "lizard", "llama", "lobster", "locket", "lotus", "lucky", "lunar", "lychee",
  "magenta", "magnet", "mango", "maple", "marble", "meadow", "melody", "melon", "mercury", "meteor", "mint", "mirror",
  "mocha", "monkey", "moose", "morning", "mosaic", "moss", "muffin", "mural", "nectar", "needle", "nest", "noble",
  "noodle", "nova", "nutmeg", "oak", "oasis", "ocean", "olive", "onion", "opal", "orange", "orbit", "orchid",
  "otter", "owl", "oyster", "paddle", "palace", "panda", "paper", "parade", "parrot", "pasta", "peach", "peanut",
  "pebble", "pecan", "pelican", "pepper", "piano", "pickle", "pigeon", "pillow", "pilot", "pine", "pixel", "planet",
  "plum", "pocket", "polar", "pony", "poppy", "potato", "prairie", "prism", "puffin", "pumpkin", "puzzle", "quartz",
  "quiet", "quill", "rabbit", "radar", "radish", "rain", "raven", "ribbon", "river", "robin", "rocket", "rose",
  "ruby", "saffron", "sage", "salmon", "sandal", "satin", "sauce", "scarf", "scout", "season", "sesame", "shadow",
  "shell", "sierra", "silver", "simple", "sketch", "sky", "sleepy", "slope", "snow", "socket", "sofa", "solar",
  "sonnet", "spark", "sparrow", "spice", "spider", "spiral", "spruce", "squid", "star", "stone", "storm", "strudel",
  "sugar", "summit", "sunny", "sunset", "swan", "sweet", "tango", "teapot", "temple", "thistle", "thunder", "tiger",
  "timber", "toast", "tomato", "topaz", "torch", "tornado", "toucan", "tulip", "tundra", "turtle", "twig", "umber",
  "valley", "velvet", "violet", "violin", "voyage", "waffle", "walnut", "walrus", "water", "wave", "willow", "window",
  "winter", "wizard", "wombat", "yarrow", "yellow", "yogurt", "zebra", "zenith", "zephyr", "zigzag",
];

// pick draws with crypto, not Math.random: a name can end up being the
// only thing keeping a preview nobody linked to from being found.
// Rejection sampling keeps every word equally likely.
function pick() {
  const span = 2 ** 32 - (2 ** 32 % WORDS.length);
  const buf = new Uint32Array(1);
  do crypto.getRandomValues(buf); while (buf[0] >= span);
  return WORDS[buf[0] % WORDS.length];
}

export function randomLabel(words) {
  const n = Math.min(MAX_WORDS, Math.max(1, Math.floor(Number(words)) || 1));
  return Array.from({ length: n }, pick).join("-");
}

// withLabel puts a fresh label in front of what the field holds, which
// is read as the parent domain. When the field still starts with the
// label made last time, that one is swapped, so pressing again rolls a
// new name instead of stacking them (a-b.c-d.example.com). A field with
// no dot holds no parent: the parent most sites already use is taken,
// and with none the label alone is a project.
export function withLabel(value, label, last, parent) {
  const v = String(value || "").trim().toLowerCase().replace(/^\.+|\.+$/g, "");
  if (last && (v === last || v.startsWith(last + "."))) return label + v.slice(last.length);
  if (v.includes(".")) return label + "." + v;
  return parent ? label + "." + parent : label;
}

// commonParent is the parent domain most of these sites hang from: the
// domain minus its first label, or the domain itself when it is only
// name and TLD (example.com is the parent of its own subdomains).
export function commonParent(domains) {
  const count = new Map();
  for (const d of domains) {
    const parts = String(d).split(".");
    if (parts.length < 2) continue;
    const p = parts.length === 2 ? d : parts.slice(1).join(".");
    count.set(p, (count.get(p) || 0) + 1);
  }
  let best = "", n = 0;
  for (const [p, c] of count) if (c > n || (c === n && p < best)) { best = p; n = c; }
  return best;
}
