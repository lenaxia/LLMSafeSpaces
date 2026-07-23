package com.llmsafespaces.sdk.exceptions;

public class NotFoundException extends LLMSafeSpacesException {
    public NotFoundException(String message) {
        super(message, 404);
    }
}
