Bun.serve({
  // `routes` requires Bun v1.2.3+
  routes: {
    // Static routes
    "/ping": new Response("OK"),
  },

  // (optional) fallback for unmatched routes:
  // Required if Bun's version < 1.2.3
  fetch(req) {
    return new Response("Not Found", { status: 404 });
  },
});
