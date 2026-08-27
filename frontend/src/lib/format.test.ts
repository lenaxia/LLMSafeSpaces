import { describe, expect, it } from "vitest";
import { formatBytes } from "./format";

describe("formatBytes", () => {
  it("formats bytes below 1 KiB", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1023)).toBe("1023 B");
  });

  it("formats KiB with no decimals", () => {
    expect(formatBytes(1024)).toBe("1 KB");
    expect(formatBytes(1536)).toBe("2 KB");
    expect(formatBytes(256 * 1024)).toBe("256 KB");
  });

  it("formats MiB with one decimal", () => {
    expect(formatBytes(1024 * 1024)).toBe("1.0 MB");
    expect(formatBytes(25 * 1024 * 1024)).toBe("25.0 MB");
  });

  it("formats GiB with one decimal", () => {
    expect(formatBytes(1024 * 1024 * 1024)).toBe("1.0 GB");
    expect(formatBytes(2.5 * 1024 * 1024 * 1024)).toBe("2.5 GB");
  });
});
