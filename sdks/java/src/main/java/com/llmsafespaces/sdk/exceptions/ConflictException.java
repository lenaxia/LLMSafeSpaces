package com.llmsafespaces.sdk.exceptions;

public class ConflictException extends LLMSafeSpacesException {
    public ConflictException(String message) {
        super(message, 409);
    }
}
