import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { SSOStartPage } from "./SSOStartPage";

vi.mock("../api/sso", () => ({
  ssoRedirectURL: (orgSlug: string) => `/api/v1/auth/sso/${orgSlug}/start`,
}));

function mockWindowLocation() {
  const hrefSetter = vi.fn();
  const originalDescriptor = Object.getOwnPropertyDescriptor(window, "location");
  Object.defineProperty(window, "location", {
    value: { href: "https://localhost/" },
    writable: true,
    configurable: true,
  });
  Object.defineProperty(window.location, "href", {
    get: () => "https://localhost/",
    set: hrefSetter,
    configurable: true,
  });
  return {
    hrefSetter,
    restore() {
      if (originalDescriptor) {
        Object.defineProperty(window, "location", originalDescriptor);
      }
    },
  };
}

function renderWithRoute(initialPath: string) {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route path="/sso/:orgSlug" element={<SSOStartPage />} />
        <Route path="/login" element={<div>Login Page</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("SSOStartPage", () => {
  it("redirects to the SSO start URL for the org slug", async () => {
    const { hrefSetter, restore } = mockWindowLocation();

    try {
      renderWithRoute("/sso/acme");
      await waitFor(() => {
        expect(hrefSetter).toHaveBeenCalledWith("/api/v1/auth/sso/acme/start");
      });
    } finally {
      restore();
    }
  });

  it("encodes the org slug in the redirect URL", async () => {
    const { hrefSetter, restore } = mockWindowLocation();

    try {
      renderWithRoute("/sso/acme-corp");
      await waitFor(() => {
        expect(hrefSetter).toHaveBeenCalledWith("/api/v1/auth/sso/acme-corp/start");
      });
    } finally {
      restore();
    }
  });

  it("shows a spinner while redirecting", () => {
    const { restore } = mockWindowLocation();

    try {
      renderWithRoute("/sso/acme");
      expect(screen.getByLabelText("Loading")).toBeInTheDocument();
    } finally {
      restore();
    }
  });

  it("navigates to /login when orgSlug is absent", () => {
    render(
      <MemoryRouter initialEntries={["/test"]}>
        <Routes>
          <Route path="/test" element={<SSOStartPage />} />
          <Route path="/login" element={<div>Login Page</div>} />
        </Routes>
      </MemoryRouter>,
    );
    expect(screen.getByText("Login Page")).toBeInTheDocument();
  });
});
