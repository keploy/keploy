package com.example.product.models;


import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor
public class Order {
    private Long id;
    private Long productId;
    private int quantity;

}