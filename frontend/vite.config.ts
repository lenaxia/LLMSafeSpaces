import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { VitePWA } from "vite-plugin-pwa";

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    VitePWA({
      registerType: "autoUpdate",
      includeAssets: ["favicon.svg"],
      manifest: {
        name: "Safe Space",
        short_name: "SafeSpace",
        description: "Chat with AI agents in isolated sandboxes",
        theme_color: "#0f172a",
        background_color: "#0f172a",
        display: "standalone",
        start_url: "/",
        icons: [
          { src: "/favicon.svg", sizes: "any", type: "image/svg+xml" },
        ],
      },
      workbox: {
        // Two categories of JS assets, with different caching contracts:
        //
        // 1. PRECACHE — core app chunks that must be atomically refreshed on
        //    every SW update. Workbox diffs the precache manifest on each SW
        //    install and re-fetches only changed entries. Any file whose
        //    staleness would break the app (app code, workbox-window itself,
        //    the Shiki web worker) must live here.
        //
        //    Includes: all non-Shiki JS emitted by the build — app entry
        //    chunks (index-*.js), vendor splits (vendor-*.js, query-*.js),
        //    vite-plugin-pwa's own workbox-window chunk, and the Shiki web
        //    worker bundle (workerBundle-*.js).
        //
        // 2. RUNTIME CacheFirst — Shiki language grammar and theme chunks.
        //    These are purely content-addressed: the filename hash changes
        //    whenever the content changes, making the old cache entry
        //    unreachable by definition. They are never invalidated by a SW
        //    update; they expire naturally after 30 days of non-use.
        //    Excluded from precache because there are ~300 of them (~9 MB);
        //    force-downloading them all at SW install time would make the
        //    first visit unusably slow.
        //
        //    The urlPattern below matches only Shiki chunks by excluding the
        //    small set of core filenames that must be precached. This is
        //    deliberate: adding a new core chunk (e.g. a new manualChunks
        //    split) requires adding its prefix to both globPatterns AND to
        //    the exclusion list in the urlPattern below so it is not
        //    accidentally routed to CacheFirst.
        globPatterns: [
          "**/*.{css,html,svg}",
          "**/index*.js",
          "**/vendor*.js",
          "**/query*.js",
          "**/workbox-window*.js",
          "**/workerBundle*.js",
        ],
        navigateFallback: "/index.html",
        navigateFallbackDenylist: [/^\/api/],
        runtimeCaching: [
          {
            urlPattern: /^\/env\.json$/,
            handler: "NetworkFirst",
            options: { cacheName: "env-config", expiration: { maxEntries: 1 } },
          },
          {
            // Shiki grammar and theme chunks — content-addressed, CacheFirst safe.
            // Excludes core chunks that are covered by the precache manifest above.
            // The regex is defined in src/lib/shiki-chunks.ts (CORE_CHUNK_PATTERN)
            // and kept in sync here — see that file for the unit tests.
            // NOTE: vite-plugin-pwa serializes this function body into sw.js at build
            // time; it does NOT bundle imports. The regex must be inline here.
            urlPattern: ({ url }: { url: URL }) =>
              url.pathname.startsWith("/assets/") &&
              url.pathname.endsWith(".js") &&
              !/\/(index|vendor|query|workbox-window|workerBundle)[^/]*\.js$/.test(url.pathname),
            handler: "CacheFirst",
            options: {
              cacheName: "shiki-chunks",
              expiration: {
                maxEntries: 350,
                maxAgeSeconds: 30 * 24 * 60 * 60,
              },
            },
          },
        ],
      },
    }),
  ],
  server: {
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
  build: {
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes("node_modules/react-dom") || id.includes("node_modules/react/") || id.includes("node_modules/react-router")) {
            return "vendor";
          }
          if (id.includes("node_modules/@tanstack/react-query")) {
            return "query";
          }
        },
      },
    },
  },
});
