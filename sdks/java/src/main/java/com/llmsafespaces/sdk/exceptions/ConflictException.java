package com.llmsafespaces.sdk.exceptions;

public class ConflictException extends LLMSafeSpacesException {
    /** Current workspace phase, when the 409 body carries one (upload phase gate, Epic 68 D5). */
    public String phase;

    public ConflictException(String message) {
        super(message, 409);
    }

    public ConflictException withPhase(String phase) {
        this.phase = phase;
        return this;
    }
}
