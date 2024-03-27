package com.example.product;

import org.junit.jupiter.api.Test;
import org.springframework.boot.test.context.SpringBootTest;

import com.example.product.commons.utils;

@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.DEFINED_PORT)
class ProductApplicationTests {

	@Test
	void contextLoads() {
	}
	@Test
	void testGetProducts(){
		
		// // Test the /products endpoint
		Boolean verifed=utils.verify("8084");
		// Verify the response
		assert(verifed.equals(true));
	}
}
