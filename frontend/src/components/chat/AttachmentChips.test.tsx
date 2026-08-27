import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { render } from "../../test/utils";
import { AttachmentChips } from "./AttachmentChips";

describe("AttachmentChips", () => {
  it("renders one chip per attachment with the display name", () => {
    render(
      <AttachmentChips
        attachments={[
          { path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt", name: "notes.txt" },
          { path: "/workspace/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-report.pdf", name: "report.pdf" },
        ]}
      />,
    );
    expect(screen.getByText("notes.txt")).toBeInTheDocument();
    expect(screen.getByText("report.pdf")).toBeInTheDocument();
    expect(screen.getAllByTestId("history-attachment-chip")).toHaveLength(2);
  });

  it("renders an empty list as nothing", () => {
    const { container } = render(<AttachmentChips attachments={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("uses the name attribute, not the path tail, for display", () => {
    render(
      <AttachmentChips
        attachments={[{ path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-pathname.txt", name: "display.txt" }]}
      />,
    );
    expect(screen.getByText("display.txt")).toBeInTheDocument();
    expect(screen.queryByText("pathname.txt")).not.toBeInTheDocument();
  });
});
